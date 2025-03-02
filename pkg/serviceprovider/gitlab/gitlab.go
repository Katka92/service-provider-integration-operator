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

package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/redhat-appstudio/remote-secret/pkg/httptransport"
	"github.com/redhat-appstudio/service-provider-integration-operator/pkg/spi-shared/config"

	"github.com/xanzy/go-gitlab"

	"k8s.io/utils/strings/slices"

	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/redhat-appstudio/service-provider-integration-operator/api/v1beta1"
	opconfig "github.com/redhat-appstudio/service-provider-integration-operator/pkg/config"
	"github.com/redhat-appstudio/service-provider-integration-operator/pkg/serviceprovider"
	"github.com/redhat-appstudio/service-provider-integration-operator/pkg/spi-shared/tokenstorage"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var unsupportedScopeError = errors.New("unsupported scope for GitLab")
var unsupportedAreaError = errors.New("unsupported permission area for GitLab")
var unsupportedUserWritePermissionError = errors.New("user write permission is not supported by GitLab")
var probeNotImplementedError = errors.New("gitLab probe not implemented")
var unexpectedStatusCodeError = errors.New("unexpected status code from GitLab API")

var publicRepoMetricConfig = serviceprovider.CommonRequestMetricsConfig(config.ServiceProviderTypeGitLab, "fetch_public_repo")
var fetchRepositoryMetricConfig = serviceprovider.CommonRequestMetricsConfig(config.ServiceProviderTypeGitLab, "fetch_single_repo")

var _ serviceprovider.ServiceProvider = (*Gitlab)(nil)

type Gitlab struct {
	Configuration          *opconfig.OperatorConfiguration
	lookup                 serviceprovider.GenericLookup
	metadataProvider       *metadataProvider
	httpClient             rest.HTTPClient
	tokenStorage           tokenstorage.TokenStorage
	glClientBuilder        serviceprovider.AuthenticatedClientBuilder[gitlab.Client]
	baseUrl                string
	downloadFileCapability downloadFileCapability
	refreshTokenCapability serviceprovider.RefreshTokenCapability
	oauthCapability        serviceprovider.OAuthCapability
	repoUrlMatcher         gitlabRepoUrlMatcher
}

var _ serviceprovider.ConstructorFunc = newGitlab

var Initializer = serviceprovider.Initializer{
	Probe:       gitlabProbe{},
	Constructor: serviceprovider.ConstructorFunc(newGitlab),
}

type gitlabOAuthCapability struct {
	serviceprovider.DefaultOAuthCapability
}

func newGitlab(factory *serviceprovider.Factory, spConfig *config.ServiceProviderConfiguration) (serviceprovider.ServiceProvider, error) {
	cache := factory.NewCacheWithExpirationPolicy(&serviceprovider.NeverMetadataExpirationPolicy{})
	glClientBuilder := gitlabClientBuilder{
		httpClient:    factory.HttpClient,
		tokenStorage:  factory.TokenStorage,
		gitlabBaseUrl: spConfig.ServiceProviderBaseUrl,
	}
	mp := &metadataProvider{
		tokenStorage:    factory.TokenStorage,
		httpClient:      factory.HttpClient,
		glClientBuilder: glClientBuilder,
		baseUrl:         spConfig.ServiceProviderBaseUrl,
	}

	var oauthCapability serviceprovider.OAuthCapability
	if spConfig.OAuth2Config != nil {
		oauthCapability = &gitlabOAuthCapability{
			DefaultOAuthCapability: serviceprovider.DefaultOAuthCapability{
				BaseUrl: factory.Configuration.BaseUrl,
			},
		}
	}

	repoUrlMatcher, err := newRepoUrlMatcher(spConfig.ServiceProviderBaseUrl)
	if err != nil {
		return nil, err
	}

	lookup := serviceprovider.GenericLookup{
		ServiceProviderType: api.ServiceProviderTypeGitLab,
		TokenFilter:         serviceprovider.NewFilter(factory.Configuration.TokenMatchPolicy, &tokenFilter{}),
		RemoteSecretFilter:  serviceprovider.DefaultRemoteSecretFilterFunc,
		MetadataProvider:    mp,
		MetadataCache:       &cache,
		RepoUrlParser:       serviceprovider.RepoUrlFromSchemalessString,
		TokenStorage:        factory.TokenStorage,
	}

	return &Gitlab{
		Configuration:    factory.Configuration,
		lookup:           lookup,
		tokenStorage:     factory.TokenStorage,
		metadataProvider: mp,
		httpClient:       factory.HttpClient,
		glClientBuilder:  glClientBuilder,
		refreshTokenCapability: refreshTokenCapability{
			httpClient:          factory.HttpClient,
			gitlabBaseUrl:       spConfig.ServiceProviderBaseUrl,
			oauthServiceBaseUrl: factory.Configuration.BaseUrl,
		},
		baseUrl:                spConfig.ServiceProviderBaseUrl,
		downloadFileCapability: NewDownloadFileCapability(factory.HttpClient, glClientBuilder, spConfig.ServiceProviderBaseUrl, repoUrlMatcher),
		oauthCapability:        oauthCapability,
		repoUrlMatcher:         repoUrlMatcher,
	}, nil
}

func (g *Gitlab) LookupTokens(ctx context.Context, cl client.Client, binding *api.SPIAccessTokenBinding) ([]api.SPIAccessToken, error) {
	tokens, err := g.lookup.Lookup(ctx, cl, binding)
	if err != nil {
		return nil, fmt.Errorf("gitlab token lookup failure: %w", err)
	}

	return tokens, nil
}

func (g *Gitlab) LookupCredentials(ctx context.Context, cl client.Client, matchable serviceprovider.Matchable) (*serviceprovider.Credentials, error) {
	credentials, err := g.lookup.LookupCredentials(ctx, cl, matchable)
	if err != nil {
		return nil, fmt.Errorf("gitlab credentials lookup failure: %w", err)
	}
	return credentials, nil
}

func (g *Gitlab) PersistMetadata(ctx context.Context, _ client.Client, token *api.SPIAccessToken) error {
	if err := g.lookup.PersistMetadata(ctx, token); err != nil {
		return fmt.Errorf("failed to persist gitlab metadata: %w", err)
	}
	return nil
}

func (g *Gitlab) GetBaseUrl() string {
	return g.baseUrl
}

func (g *Gitlab) GetDownloadFileCapability() serviceprovider.DownloadFileCapability {
	return g.downloadFileCapability
}

func (g *Gitlab) GetRefreshTokenCapability() serviceprovider.RefreshTokenCapability {
	return g.refreshTokenCapability
}

func (g *Gitlab) GetOAuthCapability() serviceprovider.OAuthCapability {
	return g.oauthCapability
}

func (g *gitlabOAuthCapability) OAuthScopesFor(permissions *api.Permissions) []string {
	// We need ScopeReadUser by default to be able to read user metadata.
	scopes := serviceprovider.GetAllScopes(translateToGitlabScopes, permissions)
	if !slices.Contains(scopes, string(ScopeReadUser)) {
		scopes = append(scopes, string(ScopeReadUser))
	}
	return scopes
}

func translateToGitlabScopes(permission api.Permission) []string {
	switch permission.Area {
	case api.PermissionAreaRepository, api.PermissionAreaRepositoryMetadata:
		if permission.Type.IsWrite() {
			return []string{string(ScopeWriteRepository)}
		}
		return []string{string(ScopeReadRepository)}
	case api.PermissionAreaRegistry:
		if permission.Type.IsWrite() {
			return []string{string(ScopeWriteRegistry)}
		}
		return []string{string(ScopeReadRegistry)}
	case api.PermissionAreaUser:
		return []string{string(ScopeReadUser)}
	}

	return []string{}
}

func (g *Gitlab) GetType() config.ServiceProviderType {
	return config.ServiceProviderTypeGitLab
}

func (g *Gitlab) CheckRepositoryAccess(ctx context.Context, cl client.Client, accessCheck *api.SPIAccessCheck) (*api.SPIAccessCheckStatus, error) {
	// We currently only check access to git repository on GitLab.
	status := &api.SPIAccessCheckStatus{
		Type:            api.SPIRepoTypeGit,
		ServiceProvider: api.ServiceProviderTypeGitLab,
		Accessibility:   api.SPIAccessCheckAccessibilityUnknown,
	}

	publicRepo, err := g.checkPublicRepoAccess(ctx, accessCheck)
	if err != nil {
		return nil, err
	}
	status.Accessible = publicRepo
	if publicRepo {
		status.Accessibility = api.SPIAccessCheckAccessibilityPublic
		return status, nil
	}

	ctx = httptransport.ContextWithMetrics(ctx, fetchRepositoryMetricConfig)
	return g.checkPrivateRepoAccess(ctx, cl, accessCheck)
}

func (g *Gitlab) checkPrivateRepoAccess(ctx context.Context, cl client.Client, accessCheck *api.SPIAccessCheck) (*api.SPIAccessCheckStatus, error) {
	status := &api.SPIAccessCheckStatus{
		Type:            api.SPIRepoTypeGit,
		ServiceProvider: api.ServiceProviderTypeGitLab,
		Accessibility:   api.SPIAccessCheckAccessibilityUnknown,
	}
	preserveError := func(errReason api.SPIAccessCheckErrorReason, err error) (*api.SPIAccessCheckStatus, error) {
		status.ErrorReason = errReason
		status.ErrorMessage = err.Error()
		return status, nil
	}

	owner, name, err := g.repoUrlMatcher.parseOwnerAndProjectFromUrl(ctx, accessCheck.Spec.RepoUrl)
	if err != nil {
		return preserveError(api.SPIAccessCheckErrorBadURL, err)
	}

	credentials, err := g.lookup.LookupCredentials(ctx, cl, accessCheck)
	if err != nil {
		return preserveError(api.SPIAccessCheckErrorTokenLookupFailed, err)
	}
	if credentials == nil {
		return status, nil
	}

	gitlabClient, err := g.glClientBuilder.CreateAuthenticatedClient(ctx, *credentials)
	if err != nil {
		return preserveError(api.SPIAccessCheckErrorUnknownError, err)
	}

	project, response, err := gitlabClient.Projects.GetProject(owner+"/"+name, nil, gitlab.WithContext(ctx))
	if err != nil {
		if response != nil && response.StatusCode == http.StatusNotFound {
			return preserveError(api.SPIAccessCheckErrorRepoNotFound, err)
		}
		return preserveError(api.SPIAccessCheckErrorUnknownError, err)
	}
	if response != nil && response.StatusCode != http.StatusOK {
		return preserveError(api.SPIAccessCheckErrorUnknownError,
			fmt.Errorf("%w: %d", unexpectedStatusCodeError, response.StatusCode))
	}

	status.Accessible = true

	// "Internal projects can be cloned by any signed-in user except external users."
	// This means that a repo cannot be accessed without user context thus the repo is not public.
	// https://docs.gitlab.com/ee/user/public_access.html#internal-projects-and-groups
	if project.Visibility == gitlab.PrivateVisibility || project.Visibility == gitlab.InternalVisibility {
		status.Accessibility = api.SPIAccessCheckAccessibilityPrivate
	}
	return status, nil
}

func (g *Gitlab) checkPublicRepoAccess(ctx context.Context, accessCheck *api.SPIAccessCheck) (bool, error) {
	ctx = httptransport.ContextWithMetrics(ctx, publicRepoMetricConfig)
	lg := log.FromContext(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, accessCheck.Spec.RepoUrl, nil)
	if err != nil {
		lg.Error(err, "failed to construct request to assess if repo is public", "accessCheck", accessCheck)
		return false, fmt.Errorf("error while constructing HTTP request for access check to %s: %w", accessCheck.Spec.RepoUrl, err)
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		lg.Error(err, "failed to request the repo to assess if it is public", "accessCheck", accessCheck)
		return false, fmt.Errorf("error performing HTTP request for access check to %s: %w", accessCheck.Spec.RepoUrl, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			lg.Error(err, "unable to close body of request for access check", "accessCheck", accessCheck)
		}
	}()

	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode != http.StatusNotFound {
		lg.Info("unexpected return code for repo", "accessCheck", accessCheck, "code", resp.StatusCode)
	}
	return false, nil
}

func (g *Gitlab) MapToken(_ context.Context, _ *api.SPIAccessTokenBinding, token *api.SPIAccessToken, tokenData *api.Token) (serviceprovider.AccessTokenMapper, error) {
	return serviceprovider.DefaultMapToken(token, tokenData), nil
}

func (g *Gitlab) Validate(_ context.Context, validated serviceprovider.Validated) (serviceprovider.ValidationResult, error) {
	ret := serviceprovider.ValidationResult{}

	for _, p := range validated.Permissions().Required {
		switch p.Area {
		case api.PermissionAreaRepository,
			api.PermissionAreaRepositoryMetadata,
			api.PermissionAreaRegistry:
			continue
		case api.PermissionAreaUser:
			if p.Type.IsWrite() {
				ret.ScopeValidation = append(ret.ScopeValidation, unsupportedUserWritePermissionError)
			}
		default:
			ret.ScopeValidation = append(ret.ScopeValidation, fmt.Errorf("%w: '%s'", unsupportedAreaError, p.Area))
		}
	}

	for _, s := range validated.Permissions().AdditionalScopes {
		if !IsValidScope(s) {
			ret.ScopeValidation = append(ret.ScopeValidation, fmt.Errorf("%w: '%s'", unsupportedScopeError, s))
		}
	}

	return ret, nil
}

type gitlabProbe struct{}

var _ serviceprovider.Probe = (*gitlabProbe)(nil)

func (p gitlabProbe) Examine(_ *http.Client, _ string) (string, error) {
	return "", probeNotImplementedError
}
