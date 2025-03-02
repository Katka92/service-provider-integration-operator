//
// Copyright (c) 2021 Red Hat, Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package serviceprovider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/redhat-appstudio/service-provider-integration-operator/pkg/spi-shared/tokenstorage"

	"github.com/redhat-appstudio/remote-secret/pkg/commaseparated"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/strings/slices"

	"github.com/redhat-appstudio/remote-secret/api/v1beta1"

	"github.com/redhat-appstudio/remote-secret/pkg/logs"

	kubeerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/redhat-appstudio/service-provider-integration-operator/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var missingTargetError = errors.New("found RemoteSecret does not have a target in the SPIAccessCheck's namespace, this should not happen")
var accessTokenNotFoundError = errors.New("token data is not found in token storage")

// GenericLookup implements a token lookup in a generic way such that the users only need to provide a function
// to provide a service-provider-specific "state" of the token and a "filter" function that uses the token and its
// state to match it against a binding
type GenericLookup struct {
	// ServiceProviderType is just the type of the provider we're dealing with. It is used to limit the number of
	// results the filter function needs to sift through.
	ServiceProviderType api.ServiceProviderType
	// TokenFilter is the filter function that decides whether a token matches the requirements of a binding, given
	// the token's service-provider-specific state
	TokenFilter        TokenFilter
	TokenStorage       tokenstorage.TokenStorage
	RemoteSecretFilter RemoteSecretFilter
	// MetadataProvider is used to figure out metadata of a token in the service provider useful for token lookup
	MetadataProvider MetadataProvider
	// MetadataCache is an abstraction used for storing/fetching the metadata of tokens
	MetadataCache *MetadataCache
	// RepoUrlParser is a function that parses URL from the repoUrl
	RepoUrlParser RepoUrlParser
}

type RepoUrlParser func(url string) (*url.URL, error)

func RepoUrlFromSchemalessString(repoUrl string) (*url.URL, error) {
	schemeIndex := strings.Index(repoUrl, "://")
	if schemeIndex == -1 {
		repoUrl = "https://" + repoUrl
	}
	return RepoUrlFromString(repoUrl)
}

func RepoUrlFromString(repoUrl string) (*url.URL, error) {
	parsed, err := url.Parse(repoUrl)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}

	return parsed, nil
}

func (l GenericLookup) Lookup(ctx context.Context, cl client.Client, matchable Matchable) ([]api.SPIAccessToken, error) {
	lg := log.FromContext(ctx)

	var result = make([]api.SPIAccessToken, 0)

	potentialMatches := &api.SPIAccessTokenList{}

	repoUrl, err := l.RepoUrlParser(matchable.RepoUrl())
	if err != nil {
		return result, fmt.Errorf("error parsing the host from repo URL %s: %w", matchable.RepoUrl(), err)
	}

	if err := cl.List(ctx, potentialMatches, client.InNamespace(matchable.ObjNamespace()), client.MatchingLabels{
		api.ServiceProviderTypeLabel: string(l.ServiceProviderType),
		api.ServiceProviderHostLabel: repoUrl.Host,
	}); err != nil {
		return result, fmt.Errorf("failed to list the potentially matching tokens: %w", err)
	}

	lg.V(logs.DebugLevel).Info("lookup", "potential_matches", len(potentialMatches.Items))

	errs := make([]error, 0)

	mutex := sync.Mutex{}
	wg := sync.WaitGroup{}
	for _, t := range potentialMatches.Items {
		if t.Status.Phase != api.SPIAccessTokenPhaseReady {
			lg.V(logs.DebugLevel).Info("skipping lookup, token not ready", "token", t.Name)
			continue
		}

		wg.Add(1)
		go func(tkn api.SPIAccessToken) {
			lg.V(logs.DebugLevel).Info("matching", "token", tkn.Name)
			defer wg.Done()
			if err := l.MetadataCache.Ensure(ctx, &tkn, l.MetadataProvider); err != nil {
				mutex.Lock()
				defer mutex.Unlock()
				lg.Error(err, "failed to refresh the metadata of candidate token", "token", tkn.Namespace+"/"+tkn.Name)
				errs = append(errs, err)
				return
			}

			ok, err := l.TokenFilter.Matches(ctx, matchable, &tkn)
			if err != nil {
				mutex.Lock()
				defer mutex.Unlock()
				lg.Error(err, "failed to match candidate token", "token", tkn.Namespace+"/"+tkn.Name)
				errs = append(errs, err)
				return
			}
			if ok {
				mutex.Lock()
				defer mutex.Unlock()
				result = append(result, tkn)
			}
		}(t)
	}

	wg.Wait()

	if len(errs) > 0 {
		return nil, fmt.Errorf("errors while examining the potential matches: %w", kubeerrors.NewAggregate(errs))
	}

	lg.V(logs.DebugLevel).Info("lookup finished", "matching_tokens", len(result))

	return result, nil
}

func (l GenericLookup) PersistMetadata(ctx context.Context, token *api.SPIAccessToken) error {
	return l.MetadataCache.Ensure(ctx, token, l.MetadataProvider)
}

// LookupCredentials tries to obtain credentials from either SPIAccessToken resource or RemoteSecret resource.
// Note that it may return, nil, nil if no suitable resource is found.
func (l GenericLookup) LookupCredentials(ctx context.Context, cl client.Client, matchable Matchable) (*Credentials, error) {
	tokens, err := l.Lookup(ctx, cl, matchable)
	if err != nil {
		return nil, err
	}

	if len(tokens) > 0 {
		tokenData, err := l.TokenStorage.Get(ctx, &tokens[0])
		if err != nil {
			return nil, fmt.Errorf("failed to get token data: %w", err)
		}
		if tokenData == nil {
			return nil, accessTokenNotFoundError
		}
		return &Credentials{Username: tokenData.Username, Token: tokenData.AccessToken}, nil
	}

	remoteSecrets, err := l.lookupRemoteSecrets(ctx, cl, matchable)
	if err != nil {
		return nil, err
	}

	secret, err := l.lookupRemoteSecretSecret(ctx, cl, matchable, remoteSecrets)
	if err != nil {
		return nil, err
	}
	if secret == nil {
		return nil, nil
	}

	return &Credentials{
		Username: string(secret.Data[v1.BasicAuthUsernameKey]),
		Token:    string(secret.Data[v1.BasicAuthPasswordKey]),
	}, nil
}

// lookupRemoteSecrets searches for RemoteSecrets with RSServiceProviderHostLabel in the same namespaces matchable and
// filters them using GenericLookup's RemoteSecretFilter.
func (l GenericLookup) lookupRemoteSecrets(ctx context.Context, cl client.Client, matchable Matchable) ([]v1beta1.RemoteSecret, error) {
	lg := log.FromContext(ctx)

	repoUrl, err := l.RepoUrlParser(matchable.RepoUrl())
	if err != nil {
		return nil, fmt.Errorf("error parsing the repo URL %s: %w", matchable.RepoUrl(), err)
	}

	potentialMatches := &v1beta1.RemoteSecretList{}
	if err := cl.List(ctx, potentialMatches, client.InNamespace(matchable.ObjNamespace()), client.MatchingLabels{
		api.RSServiceProviderHostLabel: repoUrl.Host,
	}); err != nil {
		return nil, fmt.Errorf("failed to list the potentially matching remote secrets: %w", err)
	}
	lg.V(logs.DebugLevel).Info("remote secret lookup", "potential_matches", len(potentialMatches.Items))

	matches := make([]v1beta1.RemoteSecret, 0)
	// For now let's just do a linear search. In the future we can think about go func like in Lookup.
	for i := range potentialMatches.Items {
		if l.RemoteSecretFilter == nil || l.RemoteSecretFilter.Matches(ctx, matchable, &potentialMatches.Items[i]) {
			matches = append(matches, potentialMatches.Items[i])
		}
	}

	return matches, nil
}

// lookupRemoteSecretSecret finds a matching RemoteSecret based on the repoUrl of matchable. From this RemoteSecret it
// finds and gets the target Secret from the same namespace as matchable.
func (l GenericLookup) lookupRemoteSecretSecret(ctx context.Context, cl client.Client, matchable Matchable, remoteSecrets []v1beta1.RemoteSecret) (*v1.Secret, error) {
	if len(remoteSecrets) == 0 {
		return nil, nil
	}

	repoUrl, err := l.RepoUrlParser(matchable.RepoUrl())
	if err != nil {
		return nil, fmt.Errorf("error parsing the repo URL %s: %w", matchable.RepoUrl(), err)
	}

	matchingRemoteSecret := remoteSecrets[0]
	for _, rs := range remoteSecrets {
		accessibleRepositories := rs.Annotations[api.RSServiceProviderRepositoryAnnotation]
		if slices.Contains(commaseparated.Value(accessibleRepositories).Values(), strings.TrimPrefix(repoUrl.Path, "/")) {
			matchingRemoteSecret = rs
			break
		}
	}

	targetIndex := getLocalNamespaceTargetIndex(matchingRemoteSecret.Status.Targets, matchable.ObjNamespace())
	if targetIndex < 0 || targetIndex >= len(matchingRemoteSecret.Status.Targets) {
		return nil, missingTargetError // Should not happen, but avoids panicking just in case.
	}

	secret := &v1.Secret{}
	err = cl.Get(ctx, client.ObjectKey{Namespace: matchable.ObjNamespace(), Name: matchingRemoteSecret.Status.Targets[targetIndex].SecretName}, secret)
	if err != nil {
		return nil, fmt.Errorf("unable to find Secret created by RemoteSecret: %w", err)
	}

	return secret, nil
}

// getLocalNamespaceTargetIndex is helper function which finds the index of a target in targets such that the target
// references namespace in the local cluster. If no such target exists, -1 is returned.
func getLocalNamespaceTargetIndex(targets []v1beta1.TargetStatus, namespace string) int {
	for i, target := range targets {
		if target.ApiUrl == "" && target.Error == "" && target.Namespace == namespace {
			return i
		}
	}
	return -1
}
