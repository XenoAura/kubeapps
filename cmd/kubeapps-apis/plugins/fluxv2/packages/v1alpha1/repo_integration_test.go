// Copyright 2021-2022 the Kubeapps contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/gen/core/packages/v1alpha1"
	plugins "github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/gen/core/plugins/v1alpha1"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/gen/plugins/fluxv2/packages/v1alpha1"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/fluxv2/packages/v1alpha1/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// This is an integration test: it tests the full integration of flux plugin with flux back-end
// To run these tests, enable ENABLE_FLUX_INTEGRATION_TESTS variable
// pre-requisites for these tests to run:
// 1) kind cluster with flux deployed
// 2) kubeapps apis apiserver service running with fluxv2 plug-in enabled, port forwarded to 8080, e.g.
//      kubectl -n kubeapps port-forward svc/kubeapps-internal-kubeappsapis 8080:8080
// 3) run './kind-cluster-setup.sh deploy' from testdata dir once prior to these tests

// this test is testing a scenario when a repo that takes a long time to index is added
// and while the indexing is in progress this repo is deleted by another request.
// The goal is to make sure that the events are processed by the cache fully in the order
// they were received and the cache does not end up in inconsistent state
func TestKindClusterAddThenDeleteRepo(t *testing.T) {
	_, _, err := checkEnv(t)
	if err != nil {
		t.Fatal(err)
	}

	redisCli, err := newRedisClientForIntegrationTest(t)
	if err != nil {
		t.Fatal(err)
	}

	// now load some large repos (bitnami)
	// I didn't want to store a large (10MB) copy of bitnami repo in our git,
	// so for now let it fetch from bitnami website
	name := types.NamespacedName{
		Name:      "bitnami-1",
		Namespace: "default",
	}
	if err = usesBitnamiCatalog(t); err != nil {
		t.Fatal(err)
	} else if err = kubeAddHelmRepository(t, name, "", in_cluster_bitnami_url, "", 0); err != nil {
		t.Fatal(err)
	}
	// wait until this repo reaches 'Ready' state so that long indexation process kicks in
	if err = kubeWaitUntilHelmRepositoryIsReady(t, name); err != nil {
		t.Fatal(err)
	}

	if err = kubeDeleteHelmRepository(t, name); err != nil {
		t.Fatal(err)
	}

	t.Logf("Waiting up to 30 seconds...")
	time.Sleep(30 * time.Second)

	if keys, err := redisCli.Keys(redisCli.Context(), "*").Result(); err != nil {
		t.Fatal(err)
	} else {
		if len(keys) != 0 {
			t.Fatalf("Failing due to unexpected state of the cache. Current keys: %s", keys)
		}
	}
}

func TestKindClusterRepoWithBasicAuth(t *testing.T) {
	fluxPluginClient, _, err := checkEnv(t)
	if err != nil {
		t.Fatal(err)
	}

	secretName := types.NamespacedName{
		Name:      "podinfo-basic-auth-secret-" + randSeq(4),
		Namespace: "default",
	}
	if err := kubeCreateSecretAndCleanup(t, newBasicAuthSecret(secretName, "foo", "bar")); err != nil {
		t.Fatalf("%v", err)
	}

	repoName := types.NamespacedName{
		Name:      "podinfo-basic-auth-" + randSeq(4),
		Namespace: "default",
	}
	if err := kubeAddHelmRepositoryAndCleanup(t, repoName, "", podinfo_basic_auth_repo_url, secretName.Name, 0); err != nil {
		t.Fatalf("%v", err)
	}

	// wait until this repo reaches 'Ready'
	if err := kubeWaitUntilHelmRepositoryIsReady(t, repoName); err != nil {
		t.Fatalf("%v", err)
	}

	name := types.NamespacedName{
		Name:      "test-create-admin-basic-auth",
		Namespace: "default",
	}
	grpcContext, err := newGrpcAdminContext(t, name)
	if err != nil {
		t.Fatal(err)
	}

	const maxWait = 25
	for i := 0; i <= maxWait; i++ {
		grpcContext, cancel := context.WithTimeout(grpcContext, defaultContextTimeout)
		defer cancel()
		resp, err := fluxPluginClient.GetAvailablePackageSummaries(
			grpcContext,
			&corev1.GetAvailablePackageSummariesRequest{
				Context: &corev1.Context{
					Namespace: "default",
				},
			})
		if err == nil {
			opt1 := cmpopts.IgnoreUnexported(
				corev1.GetAvailablePackageSummariesResponse{},
				corev1.AvailablePackageSummary{},
				corev1.AvailablePackageReference{},
				corev1.Context{},
				plugins.Plugin{},
				corev1.PackageAppVersion{})
			opt2 := cmpopts.SortSlices(lessAvailablePackageFunc)
			if got, want := resp, available_package_summaries_podinfo_basic_auth(repoName.Name); !cmp.Equal(got, want, opt1, opt2) {
				t.Errorf("mismatch (-want +got):\n%s", cmp.Diff(want, got, opt1, opt2))
			}
			break
		} else if i == maxWait {
			t.Fatalf("Timed out waiting for available package summaries, last response: %v, last error: [%v]", resp, err)
		} else {
			t.Logf("Waiting 2s for repository [%s] to be indexed, attempt [%d/%d]...", repoName, i+1, maxWait)
			time.Sleep(2 * time.Second)
		}
	}

	availablePackageRef := availableRef(repoName.Name+"/podinfo", repoName.Namespace)

	// first try the negative case, no auth - should fail due to not being able to
	// read secrets in all namespaces
	fluxPluginServiceAccount := types.NamespacedName{
		Name:      "test-repo-with-basic-auth",
		Namespace: "default",
	}
	grpcCtx, err := newGrpcFluxPluginContext(t, fluxPluginServiceAccount)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(grpcCtx, defaultContextTimeout)
	defer cancel()
	_, err = fluxPluginClient.GetAvailablePackageDetail(
		ctx,
		&corev1.GetAvailablePackageDetailRequest{AvailablePackageRef: availablePackageRef})
	if err == nil {
		t.Fatalf("Expected error, did not get one")
	} else if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("GetAvailablePackageDetailRequest expected: PermissionDenied, got: %v", err)
	}

	// this should succeed as it is done in the context of cluster admin
	grpcContext, cancel = context.WithTimeout(grpcContext, defaultContextTimeout)
	defer cancel()
	resp, err := fluxPluginClient.GetAvailablePackageDetail(
		grpcContext,
		&corev1.GetAvailablePackageDetailRequest{AvailablePackageRef: availablePackageRef})
	if err != nil {
		t.Fatalf("%v", err)
	}

	compareActualVsExpectedAvailablePackageDetail(
		t,
		resp.AvailablePackageDetail,
		expected_detail_podinfo_basic_auth(repoName.Name).AvailablePackageDetail)
}

func TestKindClusterAddPackageRepository(t *testing.T) {
	_, fluxPluginReposClient, err := checkEnv(t)
	if err != nil {
		t.Fatal(err)
	}

	// these will be used further on for TLS-related scenarios. Init
	// byte arrays up front so they can be re-used in multiple places later
	ca, pub, priv := getCertsForTesting(t)

	testCases := []struct {
		testName                 string
		request                  *corev1.AddPackageRepositoryRequest
		existingSecret           *apiv1.Secret
		expectedResponse         *corev1.AddPackageRepositoryResponse
		expectedStatusCode       codes.Code
		expectedReconcileFailure bool
		userManagedSecrets       bool
	}{
		{
			testName:           "add repo test (simplest case)",
			request:            add_repo_req_15,
			expectedResponse:   add_repo_expected_resp_2,
			expectedStatusCode: codes.OK,
		},
		{
			testName:           "package repository with basic auth (kubeapps managed secrets)",
			request:            add_repo_req_16,
			expectedResponse:   add_repo_expected_resp_3,
			expectedStatusCode: codes.OK,
		},
		{
			testName:                 "package repository with wrong basic auth fails",
			request:                  add_repo_req_17,
			expectedResponse:         add_repo_expected_resp_4,
			expectedStatusCode:       codes.OK,
			expectedReconcileFailure: true,
		},
		{
			testName: "package repository with basic auth and existing secret",
			request:  add_repo_req_18,
			existingSecret: newBasicAuthSecret(types.NamespacedName{
				Name:      "secret-1",
				Namespace: "default",
			}, "foo", "bar"),
			expectedResponse:   add_repo_expected_resp_5,
			expectedStatusCode: codes.OK,
			userManagedSecrets: true,
		},
		{
			testName: "package repository with basic auth and existing secret (kubeapps managed secrets)",
			request:  add_repo_req_18,
			existingSecret: newBasicAuthSecret(types.NamespacedName{
				Name:      "secret-1",
				Namespace: "default",
			}, "foo", "bar"),
			expectedStatusCode: codes.InvalidArgument,
		},
		{
			testName: "package repository with TLS",
			request:  add_repo_req_19,
			existingSecret: newTlsSecret(types.NamespacedName{
				Name:      "secret-2",
				Namespace: "default",
			}, pub, priv, ca),
			expectedResponse:   add_repo_expected_resp_5,
			expectedStatusCode: codes.OK,
			userManagedSecrets: true,
		},
		{
			testName: "package repository with TLS (kubeapps managed secrets)",
			request:  add_repo_req_19,
			existingSecret: newTlsSecret(types.NamespacedName{
				Name:      "secret-2",
				Namespace: "default",
			}, pub, priv, ca),
			expectedStatusCode: codes.InvalidArgument,
		},
		{
			testName:           "add OCI repo test (simplest case)",
			request:            add_repo_req_21,
			expectedResponse:   add_repo_expected_resp_6,
			expectedStatusCode: codes.OK,
		},
	}

	adminAcctName := types.NamespacedName{
		Name:      "test-add-repo-admin-" + randSeq(4),
		Namespace: "default",
	}
	grpcContext, err := newGrpcAdminContext(t, adminAcctName)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range testCases {
		t.Run(tc.testName, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(grpcContext, defaultContextTimeout)
			defer cancel()
			if tc.existingSecret != nil {
				if err := kubeCreateSecretAndCleanup(t, tc.existingSecret); err != nil {
					t.Fatalf("%v", err)
				}
			}
			setUserManagedSecretsAndCleanup(t, fluxPluginReposClient, ctx, tc.userManagedSecrets)

			resp, err := fluxPluginReposClient.AddPackageRepository(ctx, tc.request)
			if tc.expectedStatusCode != codes.OK {
				if status.Code(err) != tc.expectedStatusCode {
					t.Fatalf("Expected %v, got: %v", tc.expectedStatusCode, err)
				}
				return // done, nothing more to check
			} else if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				err := kubeDeleteHelmRepository(t, types.NamespacedName{
					Name:      tc.request.Name,
					Namespace: tc.request.Context.Namespace,
				})
				if err != nil {
					t.Logf("Failed to delete helm source due to [%v]", err)
				}
			})
			opt1 := cmpopts.IgnoreUnexported(
				corev1.AddPackageRepositoryResponse{},
				corev1.Context{},
				corev1.PackageRepositoryReference{},
				plugins.Plugin{},
			)
			if got, want := resp, tc.expectedResponse; !cmp.Equal(got, want, opt1) {
				t.Errorf("mismatch (-want +got):\n%s", cmp.Diff(want, got, opt1))
			}

			err = kubeWaitUntilHelmRepositoryIsReady(t, types.NamespacedName{
				Name:      tc.request.Name,
				Namespace: tc.request.Context.Namespace,
			})
			if err != nil && !tc.expectedReconcileFailure {
				t.Fatal(err)
			} else if err == nil && tc.expectedReconcileFailure {
				t.Fatalf("Expected error got nil")
			}
		})
	}
}

func TestKindClusterGetPackageRepositoryDetail(t *testing.T) {
	_, fluxPluginReposClient, err := checkEnv(t)
	if err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		testName           string
		request            *corev1.GetPackageRepositoryDetailRequest
		repoName           string
		repoType           string
		repoUrl            string
		unauthorized       bool
		expectedResponse   *corev1.GetPackageRepositoryDetailResponse
		expectedStatusCode codes.Code
		existingSecret     *apiv1.Secret
		userManagedSecrets bool
	}{
		{
			testName:           "gets detail for podinfo package repository",
			request:            get_repo_detail_req_6,
			repoName:           "my-podinfo",
			repoUrl:            podinfo_repo_url,
			expectedStatusCode: codes.OK,
			expectedResponse:   get_repo_detail_resp_11,
		},
		{
			testName:           "gets detail for bitnami package repository",
			request:            get_repo_detail_req_7,
			repoName:           "my-bitnami",
			repoUrl:            "https://charts.bitnami.com/bitnami",
			expectedStatusCode: codes.OK,
			expectedResponse:   get_repo_detail_resp_12,
		},
		{
			testName:           "get detail fails for podinfo basic auth package repository without creds",
			request:            get_repo_detail_req_9,
			repoName:           "my-podinfo-2",
			repoUrl:            podinfo_basic_auth_repo_url,
			expectedStatusCode: codes.OK,
			expectedResponse:   get_repo_detail_resp_13,
		},
		{
			testName:           "get detail succeeds for podinfo basic auth package repository with creds",
			request:            get_repo_detail_req_10,
			repoName:           "my-podinfo-3",
			repoUrl:            podinfo_basic_auth_repo_url,
			expectedStatusCode: codes.OK,
			expectedResponse:   get_repo_detail_resp_14,
			existingSecret: newBasicAuthSecret(types.NamespacedName{
				Name:      "secret-1",
				Namespace: "TBD",
			}, "foo", "bar"),
			userManagedSecrets: true,
		},
		{
			testName:           "get detail succeeds for podinfo basic auth package repository with creds (kubeapps managed secrets)",
			request:            get_repo_detail_req_10,
			repoName:           "my-podinfo-3",
			repoUrl:            podinfo_basic_auth_repo_url,
			expectedStatusCode: codes.OK,
			expectedResponse:   get_repo_detail_resp_14a,
			existingSecret: newBasicAuthSecret(types.NamespacedName{
				Name:      "secret-1",
				Namespace: "TBD",
			}, "foo", "bar"),
		},
		{
			testName:           "get detail returns NotFound error for wrong repo",
			request:            get_repo_detail_req_8,
			repoName:           "my-podinfo",
			repoUrl:            podinfo_repo_url,
			expectedStatusCode: codes.NotFound,
		},
		{
			testName:           "get detail returns PermissionDenied error",
			request:            get_repo_detail_req_11,
			repoName:           "my-podinfo-11",
			repoUrl:            podinfo_repo_url,
			expectedStatusCode: codes.PermissionDenied,
			unauthorized:       true,
		},
		{
			testName:           "returns failed status for helm repository with OCI url",
			request:            get_repo_detail_req_12,
			repoName:           "my-podinfo-12",
			repoUrl:            podinfo_oci_repo_url,
			expectedStatusCode: codes.OK,
			expectedResponse:   get_repo_detail_resp_15,
		},
		{
			testName:           "get details for OCI repo",
			request:            get_repo_detail_req_13,
			repoName:           "my-podinfo-13",
			repoType:           "oci",
			repoUrl:            podinfo_oci_repo_url,
			expectedStatusCode: codes.OK,
			expectedResponse:   get_repo_detail_resp_16,
		},
	}

	adminAcctName := types.NamespacedName{
		Name:      "test-get-repo-admin-" + randSeq(4),
		Namespace: "default",
	}
	grpcAdmin, err := newGrpcAdminContext(t, adminAcctName)
	if err != nil {
		t.Fatal(err)
	}

	loserAcctName := types.NamespacedName{
		Name:      "test-get-repo-loser-" + randSeq(4),
		Namespace: "default",
	}
	grpcLoser, err := newGrpcContextForServiceAccountWithoutAccessToAnyNamespace(t, loserAcctName)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range testCases {
		t.Run(tc.testName, func(t *testing.T) {
			repoNamespace := "test-" + randSeq(4)

			if err := kubeCreateNamespaceAndCleanup(t, repoNamespace); err != nil {
				t.Fatal(err)
			}
			secretName := ""
			if tc.existingSecret != nil {
				tc.existingSecret.Namespace = repoNamespace
				if err := kubeCreateSecretAndCleanup(t, tc.existingSecret); err != nil {
					t.Fatalf("%v", err)
				}
				secretName = tc.existingSecret.Name
			}

			if err = kubeAddHelmRepositoryAndCleanup(t, types.NamespacedName{
				Name:      tc.repoName,
				Namespace: repoNamespace,
			}, tc.repoType, tc.repoUrl, secretName, 0); err != nil {
				t.Fatal(err)
			}

			tc.request.PackageRepoRef.Context.Namespace = repoNamespace
			if tc.expectedResponse != nil {
				tc.expectedResponse.Detail.PackageRepoRef.Context.Namespace = repoNamespace
			}

			var grpcCtx context.Context
			var cancel context.CancelFunc
			if tc.unauthorized {
				grpcCtx, cancel = context.WithTimeout(grpcLoser, defaultContextTimeout)
			} else {
				grpcCtx, cancel = context.WithTimeout(grpcAdmin, defaultContextTimeout)
			}
			defer cancel()

			setUserManagedSecretsAndCleanup(t, fluxPluginReposClient, grpcCtx, tc.userManagedSecrets)

			var resp *corev1.GetPackageRepositoryDetailResponse
			for {
				grpcCtx, cancel = context.WithTimeout(grpcCtx, defaultContextTimeout)
				defer cancel()

				resp, err = fluxPluginReposClient.GetPackageRepositoryDetail(grpcCtx, tc.request)
				if got, want := status.Code(err), tc.expectedStatusCode; got != want {
					t.Fatalf("got: %v, want: %v, last repo detail: %v", err, want, resp)
				}

				if tc.expectedStatusCode != codes.OK {
					// we are done
					return
				}
				if resp.Detail.Status.Reason != corev1.PackageRepositoryStatus_STATUS_REASON_PENDING {
					break
				} else {
					t.Logf("Waiting 2s for repository reconciliation to complete...")
					time.Sleep(2 * time.Second)
				}
			}
			compareActualVsExpectedPackageRepositoryDetail(t, resp, tc.expectedResponse)
		})
	}
}

func TestKindClusterGetPackageRepositorySummaries(t *testing.T) {
	_, fluxPluginReposClient, err := checkEnv(t)
	if err != nil {
		t.Fatal(err)
	}

	type repoSpec struct {
		name string
		ns   string
		typ  string
		url  string
	}

	ns1 := "ns1-" + randSeq(4)
	ns2 := "ns2-" + randSeq(4)
	ns3 := "ns3-" + randSeq(4)

	for _, namespace := range []string{ns1, ns2, ns3} {
		ns := namespace
		if err := kubeCreateNamespaceAndCleanup(t, ns); err != nil {
			t.Fatal(err)
		}
	}

	testCases := []struct {
		testName           string
		request            *corev1.GetPackageRepositorySummariesRequest
		existingRepos      []repoSpec
		unauthorized       bool
		expectedResponse   *corev1.GetPackageRepositorySummariesResponse
		expectedStatusCode codes.Code
	}{
		{
			testName: "admin gets summaries (all namespaces)",
			request: &corev1.GetPackageRepositorySummariesRequest{
				Context: &corev1.Context{},
			},
			existingRepos: []repoSpec{
				{name: "podinfo-1", ns: ns1, url: podinfo_repo_url},
				{name: "podinfo-2", ns: ns2, url: podinfo_repo_url},
				{name: "podinfo-3", ns: ns3, url: podinfo_repo_url},
			},
			expectedStatusCode: codes.OK,
			expectedResponse: &corev1.GetPackageRepositorySummariesResponse{
				PackageRepositorySummaries: []*corev1.PackageRepositorySummary{
					get_summaries_summary_5(types.NamespacedName{Name: "podinfo-1", Namespace: ns1}),
					get_summaries_summary_5(types.NamespacedName{Name: "podinfo-2", Namespace: ns2}),
					get_summaries_summary_5(types.NamespacedName{Name: "podinfo-3", Namespace: ns3}),
				},
			},
		},
		{
			testName: "admin gets summaries (one namespace)",
			request: &corev1.GetPackageRepositorySummariesRequest{
				Context: &corev1.Context{Namespace: ns2},
			},
			existingRepos: []repoSpec{
				{name: "podinfo-4", ns: ns1, url: podinfo_repo_url},
				{name: "podinfo-5", ns: ns2, url: podinfo_repo_url},
				{name: "podinfo-6", ns: ns3, url: podinfo_repo_url},
			},
			expectedStatusCode: codes.OK,
			expectedResponse: &corev1.GetPackageRepositorySummariesResponse{
				PackageRepositorySummaries: []*corev1.PackageRepositorySummary{
					get_summaries_summary_5(types.NamespacedName{Name: "podinfo-5", Namespace: ns2}),
				},
			},
		},
		{
			testName: "loser gets summaries (one namespace => permission denined)",
			request: &corev1.GetPackageRepositorySummariesRequest{
				Context: &corev1.Context{Namespace: ns2},
			},
			existingRepos: []repoSpec{
				{name: "podinfo-7", ns: ns1, url: podinfo_repo_url},
				{name: "podinfo-8", ns: ns2, url: podinfo_repo_url},
				{name: "podinfo-9", ns: ns3, url: podinfo_repo_url},
			},
			expectedStatusCode: codes.PermissionDenied,
			unauthorized:       true,
		},
		{
			testName: "loser gets summaries (all namespaces => empty list)",
			request: &corev1.GetPackageRepositorySummariesRequest{
				Context: &corev1.Context{},
			},
			existingRepos: []repoSpec{
				{name: "podinfo-10", ns: ns1, url: podinfo_repo_url},
				{name: "podinfo-11", ns: ns2, url: podinfo_repo_url},
				{name: "podinfo-12", ns: ns3, url: podinfo_repo_url},
			},
			expectedStatusCode: codes.OK,
			expectedResponse:   &corev1.GetPackageRepositorySummariesResponse{
				// PackageRepositorySummaries: is empty so is omitted
			},
			unauthorized: true,
		},
		{
			testName: "summaries from OCI repo",
			request: &corev1.GetPackageRepositorySummariesRequest{
				Context: &corev1.Context{},
			},
			existingRepos: []repoSpec{
				{
					name: "podinfo-13",
					ns:   ns1,
					typ:  "oci",
					url:  podinfo_oci_repo_url,
				},
			},
			expectedStatusCode: codes.OK,
			expectedResponse: &corev1.GetPackageRepositorySummariesResponse{
				PackageRepositorySummaries: []*corev1.PackageRepositorySummary{
					get_summaries_summary_6(types.NamespacedName{
						Name:      "podinfo-13",
						Namespace: ns1}),
				},
			},
		},
	}

	adminAcctName := types.NamespacedName{
		Name:      "test-get-summaries-admin-" + randSeq(4),
		Namespace: "default",
	}
	grpcAdmin, err := newGrpcAdminContext(t, adminAcctName)
	if err != nil {
		t.Fatal(err)
	}

	loserAcctName := types.NamespacedName{
		Name:      "test-get-summaries-loser-" + randSeq(4),
		Namespace: "default",
	}
	grpcLoser, err := newGrpcContextForServiceAccountWithoutAccessToAnyNamespace(t, loserAcctName)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range testCases {
		t.Run(tc.testName, func(t *testing.T) {
			for _, repo := range tc.existingRepos {
				if err = kubeAddHelmRepositoryAndCleanup(t,
					types.NamespacedName{
						Name:      repo.name,
						Namespace: repo.ns}, repo.typ, repo.url, "", 0); err != nil {
					t.Fatal(err)
				}
				// want to wait until all repos reach Ready state
				err := kubeWaitUntilHelmRepositoryIsReady(t, types.NamespacedName{
					Name:      repo.name,
					Namespace: repo.ns})
				if err != nil {
					t.Fatal(err)
				}
			}

			var grpcCtx context.Context
			var cancel context.CancelFunc
			if tc.unauthorized {
				grpcCtx, cancel = context.WithTimeout(grpcLoser, defaultContextTimeout)
			} else {
				grpcCtx, cancel = context.WithTimeout(grpcAdmin, defaultContextTimeout)
			}
			defer cancel()

			resp, err := fluxPluginReposClient.GetPackageRepositorySummaries(grpcCtx, tc.request)
			if got, want := status.Code(err), tc.expectedStatusCode; got != want {
				t.Fatalf("got: %v, want: %v", err, want)
			}
			if tc.expectedStatusCode != codes.OK {
				// we are done
				return
			}

			opts := cmpopts.IgnoreUnexported(
				corev1.Context{},
				corev1.PackageRepositoryReference{},
				plugins.Plugin{},
				corev1.PackageRepositoryStatus{},
				corev1.GetPackageRepositorySummariesResponse{},
				corev1.PackageRepositorySummary{},
			)

			if got, want := resp, tc.expectedResponse; !cmp.Equal(want, got, opts) {
				t.Errorf("mismatch (-want +got):\n%s", cmp.Diff(want, got, opts, opts))
			}
		})
	}
}

func TestKindClusterUpdatePackageRepository(t *testing.T) {
	_, fluxPluginReposClient, err := checkEnv(t)
	if err != nil {
		t.Fatal(err)
	}

	ca, pub, priv := getCertsForTesting(t)

	testCases := []struct {
		name               string
		request            *corev1.UpdatePackageRepositoryRequest
		repoName           string
		repoUrl            string
		unauthorized       bool
		failed             bool
		expectedResponse   *corev1.UpdatePackageRepositoryResponse
		expectedDetail     *corev1.GetPackageRepositoryDetailResponse
		expectedStatusCode codes.Code
		oldSecret          *apiv1.Secret
		newSecret          *apiv1.Secret
		userManagedSecrets bool
	}{
		{
			name:               "update url and auth for podinfo package repository",
			request:            update_repo_req_11,
			repoName:           "my-podinfo",
			repoUrl:            podinfo_repo_url,
			expectedStatusCode: codes.OK,
			expectedResponse:   update_repo_resp_2,
			expectedDetail:     update_repo_detail_11,
		},
		{
			name:               "update package repository in a failed state",
			request:            update_repo_req_12,
			repoName:           "my-podinfo-2",
			repoUrl:            podinfo_basic_auth_repo_url,
			expectedStatusCode: codes.OK,
			expectedResponse:   update_repo_resp_3,
			expectedDetail:     update_repo_detail_12,
			failed:             true,
		},
		{
			name:               "update package repository errors when unauthorized",
			request:            update_repo_req_13,
			repoName:           "my-podinfo-3",
			repoUrl:            podinfo_repo_url,
			expectedStatusCode: codes.PermissionDenied,
			unauthorized:       true,
		},
		{
			name:               "update url and auth for podinfo package repository (user-managed secrets) errors when secret doesnt exist",
			request:            update_repo_req_14,
			repoName:           "my-podinfo-4",
			repoUrl:            podinfo_repo_url,
			expectedStatusCode: codes.NotFound,
			userManagedSecrets: true,
		},
		{
			name:     "update url and auth for podinfo package repository (user-managed secrets)",
			request:  update_repo_req_14,
			repoName: "my-podinfo-4",
			repoUrl:  podinfo_repo_url,
			newSecret: newBasicAuthSecret(types.NamespacedName{
				Name:      "secret-1",
				Namespace: "TBD",
			}, "foo", "bar"),
			expectedResponse:   update_repo_resp_4,
			expectedDetail:     update_repo_detail_13,
			userManagedSecrets: true,
		},
		{
			name:     "update repository change from TLS cert/key to basic auth (kubeapps-managed secrets)",
			request:  update_repo_req_15,
			repoName: "my-podinfo-5",
			repoUrl:  podinfo_tls_repo_url,
			oldSecret: newTlsSecret(types.NamespacedName{
				Name:      "secret-1",
				Namespace: "TBD",
			}, pub, priv, ca),
			expectedStatusCode: codes.OK,
			expectedResponse:   update_repo_resp_5,
			expectedDetail:     update_repo_detail_14,
		},
	}

	adminAcctName := types.NamespacedName{
		Name:      "test-update-repo-admin-" + randSeq(4),
		Namespace: "default",
	}
	grpcAdmin, err := newGrpcAdminContext(t, adminAcctName)
	if err != nil {
		t.Fatal(err)
	}

	loserAcctName := types.NamespacedName{
		Name:      "test-update-repo-loser-" + randSeq(4),
		Namespace: "default",
	}
	grpcLoser, err := newGrpcContextForServiceAccountWithoutAccessToAnyNamespace(t, loserAcctName)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			repoNamespace := "test-" + randSeq(4)

			if err := kubeCreateNamespaceAndCleanup(t, repoNamespace); err != nil {
				t.Fatal(err)
			}
			oldSecretName := ""
			if tc.oldSecret != nil {
				tc.oldSecret.Namespace = repoNamespace
				if err := kubeCreateSecret(t, tc.oldSecret); err != nil {
					t.Fatalf("%v", err)
				}
				oldSecretName = tc.oldSecret.GetName()
				if tc.userManagedSecrets {
					t.Cleanup(func() {
						err := kubeDeleteSecret(t, types.NamespacedName{
							Name:      tc.oldSecret.Name,
							Namespace: tc.oldSecret.Namespace,
						})
						if err != nil {
							t.Logf("Failed to delete secret [%s] due to [%v]", tc.oldSecret.Name, err)
						}
					})
				}
			}
			if tc.newSecret != nil {
				tc.newSecret.Namespace = repoNamespace
				if err := kubeCreateSecretAndCleanup(t, tc.newSecret); err != nil {
					t.Fatalf("%v", err)
				}
			}

			name := types.NamespacedName{
				Name:      tc.repoName,
				Namespace: repoNamespace,
			}
			if err = kubeAddHelmRepositoryAndCleanup(t, name,
				"", tc.repoUrl, oldSecretName, 0); err != nil {
				t.Fatal(err)
			}
			// wait until this repo reaches 'Ready' state so that long indexation process kicks in
			err := kubeWaitUntilHelmRepositoryIsReady(t, name)
			if err != nil {
				if !tc.failed {
					t.Fatalf("%v", err)
				} else {
					// sanity check : make sure repo is in failed state
					if err.Error() != "Failed: failed to fetch Helm repository index: failed to cache index to temporary file: failed to fetch http://fluxv2plugin-testdata-svc.default.svc.cluster.local:80/podinfo-basic-auth/index.yaml : 401 Unauthorized" {
						t.Fatalf("%v", err)
					}
				}
			}

			var grpcCtx context.Context
			var cancel context.CancelFunc
			if tc.unauthorized {
				grpcCtx, cancel = context.WithTimeout(grpcLoser, defaultContextTimeout)
			} else {
				grpcCtx, cancel = context.WithTimeout(grpcAdmin, defaultContextTimeout)
			}
			defer cancel()

			setUserManagedSecretsAndCleanup(t, fluxPluginReposClient, grpcCtx, tc.userManagedSecrets)

			tc.request.PackageRepoRef.Context.Namespace = repoNamespace
			if tc.expectedResponse != nil {
				tc.expectedResponse.PackageRepoRef.Context.Namespace = repoNamespace
			}
			if tc.expectedDetail != nil {
				tc.expectedDetail.Detail.PackageRepoRef.Context.Namespace = repoNamespace
			}

			// every once in a while (very infrequently) I get
			// rpc error: code = Internal desc = unable to update the HelmRepository
			// 'test-nsrp/my-podinfo-2' due to 'Operation cannot be fulfilled on
			// helmrepositories.source.toolkit.fluxcd.io "my-podinfo-2": the object has been modified;
			// please apply your changes to the latest version and try again
			// the loop below takes care of this scenario
			var i, maxRetries = 0, 5
			var resp *corev1.UpdatePackageRepositoryResponse
			for ; i < maxRetries; i++ {
				resp, err = fluxPluginReposClient.UpdatePackageRepository(grpcCtx, tc.request)
				if err != nil && strings.Contains(err.Error(), " the object has been modified; please apply your changes to the latest version and try again") {
					waitTime := int64(math.Pow(2, float64(i)))
					t.Logf("Retrying update in [%d] sec due to %s...", waitTime, err.Error())
					time.Sleep(time.Duration(waitTime) * time.Second)
				} else {
					break
				}
			}
			if i == maxRetries {
				t.Fatalf("Update retries exhaused for repository [%s], last error: [%v]", tc.repoName, err)
			}
			if got, want := status.Code(err), tc.expectedStatusCode; got != want {
				t.Fatalf("got: %v, want: %v", err, want)
			}
			if tc.expectedStatusCode != codes.OK {
				// we are done
				return
			}

			opts := cmpopts.IgnoreUnexported(
				corev1.Context{},
				corev1.PackageRepositoryReference{},
				plugins.Plugin{},
				corev1.UpdatePackageRepositoryResponse{},
			)

			if got, want := resp, tc.expectedResponse; !cmp.Equal(want, got, opts) {
				t.Errorf("mismatch (-want +got):\n%s", cmp.Diff(want, got, opts, opts))
			}

			actualDetail := waitForRepoToReconcileWithSuccess(
				t, fluxPluginReposClient, grpcCtx, tc.repoName, repoNamespace)
			compareActualVsExpectedPackageRepositoryDetail(t, actualDetail, tc.expectedDetail)
		})
	}
}

func TestKindClusterDeletePackageRepository(t *testing.T) {
	_, fluxPluginReposClient, err := checkEnv(t)
	if err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		name               string
		request            *corev1.DeletePackageRepositoryRequest
		repoName           string
		repoUrl            string
		unauthorized       bool
		failed             bool
		expectedStatusCode codes.Code
		oldSecret          *apiv1.Secret
		newSecret          *apiv1.Secret
		userManagedSecrets bool
	}{
		{
			name:               "basic delete of package repository",
			request:            delete_repo_req_3,
			repoName:           "my-podinfo",
			repoUrl:            podinfo_repo_url,
			expectedStatusCode: codes.OK,
		},
		{
			name:               "delete package repository in a failed state",
			request:            delete_repo_req_4,
			repoName:           "my-podinfo-2",
			repoUrl:            podinfo_basic_auth_repo_url,
			expectedStatusCode: codes.OK,
			failed:             true,
		},
		{
			name:               "delete package repository errors when unauthorized",
			request:            delete_repo_req_5,
			repoName:           "my-podinfo-3",
			repoUrl:            podinfo_repo_url,
			expectedStatusCode: codes.PermissionDenied,
			unauthorized:       true,
		},
		{ //TODO rewrite this test to use AddPackageRepository
			//Instead of kubeAddHelmRepository so we don't need to copy
			//production code bizness logic here
			name:     "delete repo also deletes the corresponding secret in kubeapps managed env",
			request:  delete_repo_req_6,
			repoName: "my-podinfo-4",
			repoUrl:  podinfo_basic_auth_repo_url,
			oldSecret: newBasicAuthSecret(types.NamespacedName{
				Name:      "secret-1",
				Namespace: "namespace-1"}, "foo", "bar"),
			expectedStatusCode: codes.OK,
		},
	}

	adminAcctName := types.NamespacedName{
		Name:      "test-delete-repo-admin-" + randSeq(4),
		Namespace: "default",
	}
	grpcAdmin, err := newGrpcAdminContext(t, adminAcctName)
	if err != nil {
		t.Fatal(err)
	}

	loserAcctName := types.NamespacedName{
		Name:      "test-delete-repo-loser-" + randSeq(4),
		Namespace: "default",
	}
	grpcLoser, err := newGrpcContextForServiceAccountWithoutAccessToAnyNamespace(t, loserAcctName)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			repoNamespace := "test-" + randSeq(4)

			if err := kubeCreateNamespaceAndCleanup(t, repoNamespace); err != nil {
				t.Fatal(err)
			}
			oldSecretName := ""
			if tc.oldSecret != nil {
				tc.oldSecret.Namespace = repoNamespace
				if err := kubeCreateSecret(t, tc.oldSecret); err != nil {
					t.Fatalf("%v", err)
				}
				oldSecretName = tc.oldSecret.GetName()
				if tc.userManagedSecrets {
					t.Cleanup(func() {
						err := kubeDeleteSecret(t,
							types.NamespacedName{
								Namespace: tc.oldSecret.Namespace,
								Name:      tc.oldSecret.Name})
						if err != nil {
							t.Logf("Failed to delete secret [%s] due to [%v]", tc.oldSecret.Name, err)
						}
					})
				}
			}
			if tc.newSecret != nil {
				tc.newSecret.Namespace = repoNamespace
				if err := kubeCreateSecretAndCleanup(t, tc.newSecret); err != nil {
					t.Fatalf("%v", err)
				}
			}

			name := types.NamespacedName{
				Name:      tc.repoName,
				Namespace: repoNamespace,
			}
			if err = kubeAddHelmRepository(t, name, "", tc.repoUrl, oldSecretName, 0); err != nil {
				t.Fatal(err)
				// wait until this repo reaches 'Ready' state so that long indexation process kicks in
			} else if !tc.userManagedSecrets && tc.oldSecret != nil {
				if repo, err := kubeGetHelmRepository(t, name); err != nil {
					t.Fatal(err)
				} else if err = kubeSetSecretOwnerRef(t, types.NamespacedName{
					Namespace: tc.oldSecret.Namespace,
					Name:      tc.oldSecret.Name}, repo); err != nil {
					t.Fatal(err)
				}
			} else if err := kubeWaitUntilHelmRepositoryIsReady(t, name); err != nil {
				if !tc.failed {
					t.Fatal(err)
				} else {
					// sanity check : make sure repo is in failed state
					if err.Error() != "Failed: failed to fetch Helm repository index: failed to cache index to temporary file: failed to fetch http://fluxv2plugin-testdata-svc.default.svc.cluster.local:80/podinfo-basic-auth/index.yaml : 401 Unauthorized" {
						t.Fatal(err)
					}
				}
			}

			var grpcCtx context.Context
			var cancel context.CancelFunc
			if tc.unauthorized {
				grpcCtx, cancel = context.WithTimeout(grpcLoser, defaultContextTimeout)
			} else {
				grpcCtx, cancel = context.WithTimeout(grpcAdmin, defaultContextTimeout)
			}
			defer cancel()

			setUserManagedSecretsAndCleanup(t, fluxPluginReposClient, grpcCtx, tc.userManagedSecrets)

			tc.request.PackageRepoRef.Context.Namespace = repoNamespace

			_, err = fluxPluginReposClient.DeletePackageRepository(grpcCtx, tc.request)
			if got, want := status.Code(err), tc.expectedStatusCode; got != want {
				t.Fatalf("got: %v, want: %v", err, want)
			}

			if tc.expectedStatusCode != codes.OK {
				// we are done
				return
			}

			const maxWait = 25
			for i := 0; i <= maxWait; i++ {
				exists, err := kubeExistsHelmRepository(t, name)
				if err != nil {
					t.Fatal(err)
				} else if !exists {
					break
				} else if i == maxWait {
					t.Fatalf("Timed out waiting for delete of repository [%s], last error: [%v]", tc.repoName, err)
				} else {
					t.Logf("Waiting 1s for repository [%s] to be deleted, attempt [%d/%d]...", tc.repoName, i+1, maxWait)
					time.Sleep(1 * time.Second)
				}
			}

			// check the secret is gone too in kubeapps-managed secrets env
			if !tc.userManagedSecrets && tc.oldSecret != nil {
				for i := 0; i <= maxWait; i++ {
					exists, err := kubeExistsSecret(t, types.NamespacedName{
						Name:      tc.oldSecret.Name,
						Namespace: repoNamespace,
					})
					if err != nil {
						t.Fatal(err)
					} else if !exists {
						break
					} else if i == maxWait {
						t.Fatalf("Timed out waiting for delete of secret [%s], last error: [%v]", tc.oldSecret.Name, err)
					} else {
						t.Logf("Waiting 1s for secret [%s] to be deleted, attempt [%d/%d]...", tc.oldSecret.Name, i+1, maxWait)
						time.Sleep(1 * time.Second)
					}
				}
			}
		})
	}
}

func TestKindClusterUpdatePackageRepoSecretUnchanged(t *testing.T) {
	_, fluxPluginReposClient, err := checkEnv(t)
	if err != nil {
		t.Fatal(err)
	}

	request := update_repo_req_17
	repoName := "my-podinfo-6"
	repoUrl := podinfo_basic_auth_repo_url
	oldSecret := newBasicAuthSecret(types.NamespacedName{
		Name:      "secret-1",
		Namespace: "TBD"}, "foo", "bar")
	expectedStatusCode := codes.OK
	expectedResponse := update_repo_resp_6
	expectedDetail := update_repo_detail_16
	repoNamespace := "test-" + randSeq(4)
	adminAcctName := types.NamespacedName{
		Name:      "test-update-repo-admin-" + randSeq(4),
		Namespace: "default",
	}

	grpcAdmin, err := newGrpcAdminContext(t, adminAcctName)
	if err != nil {
		t.Fatal(err)
	}

	if err := kubeCreateNamespaceAndCleanup(t, repoNamespace); err != nil {
		t.Fatal(err)
	}
	oldSecretName := ""
	if oldSecret != nil {
		oldSecret.Namespace = repoNamespace
		if err := kubeCreateSecret(t, oldSecret); err != nil {
			t.Fatal(err)
		}
		oldSecretName = oldSecret.GetName()
	}

	name := types.NamespacedName{
		Name:      repoName,
		Namespace: repoNamespace,
	}
	if err = kubeAddHelmRepositoryAndCleanup(t, name, "", repoUrl, oldSecretName, 0); err != nil {
		t.Fatal(err)
	} else if err = kubeWaitUntilHelmRepositoryIsReady(t, name); err != nil {
		t.Fatal(err)
	}

	grpcCtx, cancel := context.WithTimeout(grpcAdmin, defaultContextTimeout)
	defer cancel()

	setUserManagedSecretsAndCleanup(t, fluxPluginReposClient, grpcCtx, false)

	request.PackageRepoRef.Context.Namespace = repoNamespace
	expectedResponse.PackageRepoRef.Context.Namespace = repoNamespace
	expectedDetail.Detail.PackageRepoRef.Context.Namespace = repoNamespace

	repoBeforeUpdate, err := kubeGetHelmRepository(t, types.NamespacedName{
		Name:      repoName,
		Namespace: repoNamespace,
	})
	if err != nil {
		t.Fatal(err)
	}
	repoVersionBeforeUpdate := repoBeforeUpdate.ResourceVersion
	secretNameBeforeUpdate := repoBeforeUpdate.Spec.SecretRef.Name
	secretBeforeUpdate, err := kubeGetSecret(t, types.NamespacedName{
		Namespace: repoNamespace,
		Name:      secretNameBeforeUpdate})
	if err != nil {
		t.Fatal(err)
	}
	if secretBeforeUpdate.Type != apiv1.SecretTypeOpaque {
		t.Fatalf("Unexpected secret type: %s", secretBeforeUpdate.Type)
	}
	secretVersionBeforeUpdate := secretBeforeUpdate.ResourceVersion

	// every once in a while (very infrequently) I get
	// rpc error: code = Internal desc = unable to update the HelmRepository
	// 'test-nsrp/my-podinfo-2' due to 'Operation cannot be fulfilled on
	// helmrepositories.source.toolkit.fluxcd.io "my-podinfo-2": the object has been modified;
	// please apply your changes to the latest version and try again
	// the loop below takes care of this scenario
	var i, maxRetries = 0, 5
	var resp *corev1.UpdatePackageRepositoryResponse
	for ; i < maxRetries; i++ {
		resp, err = fluxPluginReposClient.UpdatePackageRepository(grpcCtx, request)
		if err != nil && strings.Contains(err.Error(), " the object has been modified; please apply your changes to the latest version and try again") {
			waitTime := int64(math.Pow(2, float64(i)))
			t.Logf("Retrying update in [%d] sec due to %s...", waitTime, err.Error())
			time.Sleep(time.Duration(waitTime) * time.Second)
		} else {
			break
		}
	}
	if i == maxRetries {
		t.Fatalf("Update retries exhaused for repository [%s], last error: [%v]", repoName, err)
	} else if got, want := status.Code(err), expectedStatusCode; got != want {
		t.Fatalf("got: %v, want: %v", err, want)
	}

	repoAfterUpdate, err := kubeGetHelmRepository(t, types.NamespacedName{
		Name:      repoName,
		Namespace: repoNamespace})
	if err != nil {
		t.Fatal(err)
	}
	repoVersionAfterUpdate := repoAfterUpdate.ResourceVersion
	if repoVersionBeforeUpdate == repoVersionAfterUpdate {
		t.Fatalf("Expected repo version be different update")
	}
	secretNameAfterUpdate := repoAfterUpdate.Spec.SecretRef.Name
	if secretNameAfterUpdate != secretNameBeforeUpdate {
		t.Fatalf("Expected secret to be the same after update")
	}
	secretAfterUpdate, err := kubeGetSecret(t, types.NamespacedName{
		Name:      oldSecretName,
		Namespace: repoNamespace,
	})
	if err != nil {
		t.Fatal(err)
	}
	secretVersionAfterUpdate := secretAfterUpdate.ResourceVersion
	if secretVersionAfterUpdate != secretVersionBeforeUpdate {
		t.Fatalf("Expected secret version to be the same after update")
	}

	opts := cmpopts.IgnoreUnexported(
		corev1.Context{},
		corev1.PackageRepositoryReference{},
		plugins.Plugin{},
		corev1.UpdatePackageRepositoryResponse{},
	)

	if got, want := resp, expectedResponse; !cmp.Equal(want, got, opts) {
		t.Errorf("mismatch (-want +got):\n%s", cmp.Diff(want, got, opts))
	}

	actualDetail := waitForRepoToReconcileWithSuccess(
		t, fluxPluginReposClient, grpcCtx, repoName, repoNamespace)
	compareActualVsExpectedPackageRepositoryDetail(t, actualDetail, expectedDetail)
}

func compareActualVsExpectedPackageRepositoryDetail(t *testing.T, actualDetail *corev1.GetPackageRepositoryDetailResponse, expectedDetail *corev1.GetPackageRepositoryDetailResponse) {
	opts1 := cmpopts.IgnoreUnexported(
		corev1.Context{},
		corev1.PackageRepositoryReference{},
		plugins.Plugin{},
		corev1.GetPackageRepositoryDetailResponse{},
		corev1.PackageRepositoryDetail{},
		corev1.PackageRepositoryStatus{},
		corev1.PackageRepositoryAuth{},
		corev1.PackageRepositoryTlsConfig{},
		corev1.SecretKeyReference{},
		corev1.UsernamePassword{},
	)

	opts2 := cmpopts.IgnoreFields(corev1.PackageRepositoryStatus{}, "UserReason")

	if got, want := actualDetail, expectedDetail; !cmp.Equal(want, got, opts1, opts2) {
		t.Fatalf("mismatch (-want +got):\n%s", cmp.Diff(want, got, opts1, opts2))
	}

	if !strings.HasPrefix(actualDetail.GetDetail().Status.UserReason, expectedDetail.Detail.Status.UserReason) {
		t.Errorf("unexpected response (status.UserReason): (-want +got):\n- %s\n+ %s",
			expectedDetail.Detail.Status.UserReason,
			actualDetail.GetDetail().Status.UserReason)
	}
}

func setUserManagedSecretsAndCleanup(t *testing.T, fluxPluginReposClient v1alpha1.FluxV2RepositoriesServiceClient, ctx context.Context, value bool) {
	oldValue, err := fluxPluginReposClient.SetUserManagedSecrets(
		ctx, &v1alpha1.SetUserManagedSecretsRequest{Value: value})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		fluxPluginReposClient.SetUserManagedSecrets(
			ctx, &v1alpha1.SetUserManagedSecretsRequest{Value: oldValue.Value})
	})
}

func waitForRepoToReconcileWithSuccess(t *testing.T, fluxPluginReposClient v1alpha1.FluxV2RepositoriesServiceClient, ctx context.Context, name, namespace string) *corev1.GetPackageRepositoryDetailResponse {
	var actualDetail *corev1.GetPackageRepositoryDetailResponse
	var err error
	for i := 0; i < 10; i++ {
		actualDetail, err = fluxPluginReposClient.GetPackageRepositoryDetail(
			ctx,
			&corev1.GetPackageRepositoryDetailRequest{
				PackageRepoRef: &corev1.PackageRepositoryReference{
					Context: &corev1.Context{
						Namespace: namespace,
					},
					Identifier: name,
				},
			})
		if got, want := status.Code(err), codes.OK; got != want {
			t.Fatalf("got: %v, want: %v", err, want)
		}
		if actualDetail.Detail.Status.Reason == corev1.PackageRepositoryStatus_STATUS_REASON_SUCCESS {
			break
		} else {
			t.Logf("Waiting 2s for repository reconciliation to complete successfully...")
			time.Sleep(2 * time.Second)
		}
	}
	if actualDetail.Detail.Status.Reason != corev1.PackageRepositoryStatus_STATUS_REASON_SUCCESS {
		repo, _ := kubeGetHelmRepository(t, types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		})
		t.Fatalf("Timed out waiting for repository [%q] reconcile successfully after the update:\n%s",
			name, common.PrettyPrint(repo))
	}
	return actualDetail
}
