// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/k8s-config-connector/config/tests/samples/create"
	opcorev1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/operator/pkg/apis/core/v1beta1"
	"github.com/GoogleCloudPlatform/k8s-config-connector/pkg/test"
	testcontroller "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/test/controller"
	testgcp "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/test/gcp"
	"github.com/GoogleCloudPlatform/k8s-config-connector/pkg/test/resourcefixture"
	testvariable "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/test/resourcefixture/variable"
	testyaml "github.com/GoogleCloudPlatform/k8s-config-connector/pkg/test/yaml"

	"gopkg.in/dnaeon/go-vcr.v3/cassette"
	"gopkg.in/dnaeon/go-vcr.v3/recorder"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestAllInSeries(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("RUN_E2E not set; skipping")
	}

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() {
		cancel()
	})

	subtestTimeout := time.Hour
	if targetGCP := os.Getenv("E2E_GCP_TARGET"); targetGCP == "mock" {
		// We allow a total of 3 minutes: 2 for the test itself (for deep object chains with retries),
		// and 1 minute to shutdown envtest / allow kube-apiserver requests to time-out.
		subtestTimeout = 3 * time.Minute
	}

	t.Run("samples", func(t *testing.T) {
		samples := create.ListAllSamples(t)

		for _, sampleKey := range samples {
			sampleKey := sampleKey
			// TODO(b/259496928): Randomize the resource names for parallel execution when/if needed.

			t.Run(sampleKey.Name, func(t *testing.T) {
				ctx := addTestTimeout(ctx, t, subtestTimeout)

				// Quickly load the sample with a dummy project, just to see if we should skip it
				{
					dummySample := create.LoadSample(t, sampleKey, testgcp.GCPProject{ProjectID: "test-skip", ProjectNumber: 123456789})
					create.MaybeSkip(t, sampleKey.Name, dummySample.Resources)
				}

				h := create.NewHarness(ctx, t)
				project := h.Project
				s := create.LoadSample(t, sampleKey, project)

				create.SetupNamespacesAndApplyDefaults(h, s.Resources, project)

				// Hack: set project-id because mockkubeapiserver does not support webhooks
				for _, u := range s.Resources {
					annotations := u.GetAnnotations()
					if annotations == nil {
						annotations = make(map[string]string)
					}
					annotations["cnrm.cloud.google.com/project-id"] = project.ProjectID
					u.SetAnnotations(annotations)
				}

				create.RunCreateDeleteTest(h, create.CreateDeleteTestOptions{Create: s.Resources, CleanupResources: true})
			})
		}
	})

	testFixturesInSeries(ctx, t, false, cancel)
}

// TestPauseInSeries is a basic smoke test to prove that if CC pauses actuation of resources
// via the actuationMode field, then resources are not actuated onto the cloud provider.
// The current test is to make sure that POST requests are not recorded as HTTP events.
func TestPauseInSeries(t *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() {
		cancel()
	})

	testFixturesInSeries(ctx, t, true, cancel)
}

func testFixturesInSeries(ctx context.Context, t *testing.T, testPause bool, cancel context.CancelFunc) {
	t.Helper()

	subtestTimeout := time.Hour
	if targetGCP := os.Getenv("E2E_GCP_TARGET"); targetGCP == "mock" {
		// We allow a total of 3 minutes: 2 for the test itself (for deep object chains with retries),
		// and 1 minute to shutdown envtest / allow kube-apiserver requests to time-out.
		subtestTimeout = 3 * time.Minute
	}
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("RUN_E2E not set; skipping")
	}
	if testPause && os.Getenv("GOLDEN_REQUEST_CHECKS") == "" {
		t.Skip("GOLDEN_REQUEST_CHECKS not set; skipping as this test relies on the golden files.")
	}

	t.Run("fixtures", func(t *testing.T) {
		fixtures := resourcefixture.Load(t)
		for _, fixture := range fixtures {
			fixture := fixture
			// TODO(b/259496928): Randomize the resource names for parallel execution when/if needed.

			t.Run(fixture.Name, func(t *testing.T) {
				ctx := addTestTimeout(ctx, t, subtestTimeout)

				loadFixture := func(project testgcp.GCPProject, uniqueID string) (*unstructured.Unstructured, create.CreateDeleteTestOptions) {
					primaryResource := bytesToUnstructured(t, fixture.Create, uniqueID, project)

					opt := create.CreateDeleteTestOptions{CleanupResources: true}

					if fixture.Dependencies != nil {
						dependencyYamls := testyaml.SplitYAML(t, fixture.Dependencies)
						for _, dependBytes := range dependencyYamls {
							depUnstruct := bytesToUnstructured(t, dependBytes, uniqueID, project)
							opt.Create = append(opt.Create, depUnstruct)
						}
					}

					opt.Create = append(opt.Create, primaryResource)

					if fixture.Update != nil {
						u := bytesToUnstructured(t, fixture.Update, uniqueID, project)
						opt.Updates = append(opt.Updates, u)
					}
					return primaryResource, opt
				}

				runScenario(ctx, t, testPause, fixture, loadFixture)
			})
		}
	})

	// Do a cleanup while we can still handle the error.
	t.Logf("shutting down manager")
	cancel()
}

func runScenario(ctx context.Context, t *testing.T, testPause bool, fixture resourcefixture.ResourceFixture, loadFixture func(project testgcp.GCPProject, uniqueID string) (*unstructured.Unstructured, create.CreateDeleteTestOptions)) {
	// Extra indentation to avoid merge conflicts
	{
		{
			{
				uniqueID := testvariable.NewUniqueID()

				// Quickly load the fixture with a dummy project, just to see if we should skip it
				{
					_, opt := loadFixture(testgcp.GCPProject{ProjectID: "test-skip", ProjectNumber: 123456789}, uniqueID)
					create.MaybeSkip(t, fixture.Name, opt.Create)
					if testPause && containsCCOrCCC(opt.Create) {
						t.Skipf("test case %q contains ConfigConnector or ConfigConnectorContext object(s): "+
							"pause test should not run against test cases already contain ConfigConnector "+
							"or ConfigConnectorContext objects", fixture.Name)
					}
				}

				// Create test harness
				var h *create.Harness
				if os.Getenv("E2E_GCP_TARGET") == "vcr" {
					h = create.NewHarnessWithOptions(ctx, t, &create.HarnessOptions{VCRPath: fixture.SourceDir})
					hash := func(s string) uint64 {
						h := fnv.New64a()
						h.Write([]byte(s))
						return h.Sum64()
					}
					uniqueID = strconv.FormatUint(hash(t.Name()), 36)
					// Stop recording after tests finish and write to cassette
					t.Cleanup(func() {
						err := h.VCRRecorderDCL.Stop()
						if err != nil {
							t.Errorf("[VCR] Failed stop DCL vcr recorder: %v", err)
						}
						err = h.VCRRecorderTF.Stop()
						if err != nil {
							t.Errorf("[VCR] Failed stop TF vcr recorder: %v", err)
						}
						err = h.VCRRecorderOauth.Stop()
						if err != nil {
							t.Errorf("[VCR] Failed stop Oauth vcr recorder: %v", err)
						}
					})
					configureVCR(t, h)
				} else {
					h = create.NewHarness(ctx, t)
				}
				project := h.Project

				if testPause {
					// we need to modify CC/ CCC state
					createPausedCC(ctx, t, h.GetClient())
				}

				primaryResource, opt := loadFixture(project, uniqueID)

				exportResources := []*unstructured.Unstructured{primaryResource}

				create.SetupNamespacesAndApplyDefaults(h, opt.Create, project)

				opt.CleanupResources = false // We delete explicitly below
				if testPause {
					opt.SkipWaitForReady = true // Paused resources don't send out an event yet.
				}
				if os.Getenv("GOLDEN_REQUEST_CHECKS") != "" {
					// If we're doing golden request checks, create synchronously so that it is reproducible.
					// Note that this does introduce a dependency that objects are ordered correctly for creation.
					opt.CreateInOrder = true
				}
				create.RunCreateDeleteTest(h, opt)

				if os.Getenv("GOLDEN_OBJECT_CHECKS") != "" {
					for _, obj := range exportResources {
						// Get testName from t.Name()
						// If t.Name() = TestAllInInSeries_fixtures_computenodetemplate
						// the testName should be computenodetemplate
						pieces := strings.Split(t.Name(), "/")
						var testName string
						if len(pieces) > 0 {
							testName = pieces[len(pieces)-1]
						} else {
							t.Errorf("failed to get test name")
						}
						// Golden test exported GCP object
						exportedYAML := exportResource(h, obj)
						if exportedYAML != "" {
							exportedObj := &unstructured.Unstructured{}
							if err := yaml.Unmarshal([]byte(exportedYAML), exportedObj); err != nil {
								t.Fatalf("error from yaml.Unmarshal: %v", err)
							}
							if err := normalizeObject(exportedObj, project, uniqueID); err != nil {
								t.Fatalf("error from normalizeObject: %v", err)
							}
							got, err := yaml.Marshal(exportedObj)
							if err != nil {
								t.Errorf("failed to convert KRM object to yaml: %v", err)
							}

							expectedPath := filepath.Join(fixture.SourceDir, fmt.Sprintf("_generated_export_%v.golden", testName))
							h.CompareGoldenFile(expectedPath, string(got), IgnoreComments)
						}
						// Golden test created KRM object
						u := &unstructured.Unstructured{}
						u.SetGroupVersionKind(obj.GroupVersionKind())
						id := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
						if err := h.GetClient().Get(ctx, id, u); err != nil {
							t.Errorf("failed to get KRM object: %v", err)
						} else {
							if err := normalizeObject(u, project, uniqueID); err != nil {
								t.Fatalf("error from normalizeObject: %v", err)
							}
							got, err := yaml.Marshal(u)
							if err != nil {
								t.Errorf("failed to convert KRM object to yaml: %v", err)
							}
							expectedPath := filepath.Join(fixture.SourceDir, fmt.Sprintf("_generated_object_%v.golden.yaml", testName))
							test.CompareGoldenObject(t, expectedPath, got)
						}
					}
				}

				if testPause {
					opt.SkipWaitForDelete = true
				}
				if os.Getenv("GOLDEN_REQUEST_CHECKS") != "" {
					// If we're doing golden request checks, delete synchronously so that it is reproducible.
					// Note that this does introduce a dependency that objects are ordered correctly for deletion.
					opt.DeleteInOrder = true
				}
				create.DeleteResources(h, opt)

				// Verify kube events
				if h.KubeEvents != nil {
					verifyKubeWatches(h)
				}

				// Verify events against golden file
				if os.Getenv("GOLDEN_REQUEST_CHECKS") != "" {
					events := test.LogEntries(h.Events.HTTPEvents)

					operationIDs := map[string]bool{}
					networkIDs := map[string]bool{}
					pathIDs := map[string]string{}

					extractIDsFromLinks := func(link string) {
						tokens := strings.Split(link, "/")
						n := len(tokens)
						if n >= 2 {
							kind := tokens[n-2]
							id := tokens[n-1]
							switch kind {
							case "tensorboards":
								pathIDs[id] = "${tensorboardID}"
							case "tagKeys":
								pathIDs[id] = "${tagKeyID}"
							case "tagValues":
								pathIDs[id] = "${tagValueID}"
							case "datasets":
								pathIDs[id] = "${datasetID}"
							case "notificationChannels":
								pathIDs[id] = "${notificationChannelID}"
							case "alertPolicies":
								pathIDs[id] = "${alertPolicyID}"
							case "conditions":
								pathIDs[id] = "${conditionID}"
							case "operations":
								operationIDs[id] = true
								pathIDs[id] = "${operationID}"
							}
						}
					}

					// Find "easy" operations and resources by looking for fully-qualified methods
					for _, event := range events {
						u := event.Request.URL
						if index := strings.Index(u, "?"); index != -1 {
							u = u[:index]
						}
						extractIDsFromLinks(u)
					}

					for _, event := range events {
						id := ""
						body := event.Response.ParseBody()
						val, ok := body["name"]
						if ok {
							s := val.(string)
							// operation name format: operations/{operationId}
							if strings.HasPrefix(s, "operations/") {
								id = strings.TrimPrefix(s, "operations/")
							}
							// operation name format: {prefix}/operations/{operationId}
							if ix := strings.Index(s, "/operations/"); ix != -1 {
								id = strings.TrimPrefix(s[ix:], "/operations/")
							}
							// operation name format: operation-{operationId}
							if strings.HasPrefix(s, "operation") {
								id = s
							}
						}
						if id != "" {
							operationIDs[id] = true
						}
					}

					for _, event := range events {
						body := event.Response.ParseBody()
						if val, ok := body["selfLinkWithId"]; ok {
							s := val.(string)
							// self link name format: {prefix}/networks/{networksId}
							if ix := strings.Index(s, "/networks/"); ix != -1 {
								id := strings.TrimPrefix(s[ix:], "/networks/")
								networkIDs[id] = true
							}
						}

						if conditions, _, _ := unstructured.NestedSlice(body, "conditions"); conditions != nil {
							for _, conditionAny := range conditions {
								condition := conditionAny.(map[string]any)
								name, _, _ := unstructured.NestedString(condition, "name")
								if name != "" {
									extractIDsFromLinks(name)
								}
							}
						}

						if val, ok := body["projectNumber"]; ok {
							s := val.(string)
							pathIDs[s] = "${projectNumber}"
						}
					}

					for _, event := range events {
						if !strings.Contains(event.Request.URL, "/operations/${operationID}") {
							continue
						}
						responseBody := event.Response.ParseBody()
						if responseBody == nil {
							continue
						}
						name, _, _ := unstructured.NestedString(responseBody, "response", "name")
						if strings.HasPrefix(name, "tagKeys/") {
							pathIDs[name] = "tagKeys/${tagKeyID}"
						}
						if strings.HasPrefix(name, "tagValues/") {
							pathIDs[name] = "tagValues/${tagValueId}"
						}
					}

					// Replace any dynamic IDs that appear in URLs
					for _, event := range events {
						url := event.Request.URL
						for k, v := range pathIDs {
							url = strings.ReplaceAll(url, "/"+k, "/"+v)
						}
						event.Request.URL = url
					}

					// Remove operation polling requests (ones where the operation is not ready)
					events = events.KeepIf(func(e *test.LogEntry) bool {
						if !strings.Contains(e.Request.URL, "/operations/${operationID}") {
							return true
						}
						responseBody := e.Response.ParseBody()
						if responseBody == nil {
							return true
						}
						if done, _, _ := unstructured.NestedBool(responseBody, "done"); done {
							return true
						}
						// remove if not done - and done can be omitted when false
						return false
					})

					jsonMutators := []test.JSONMutator{}
					addReplacement := func(path string, newValue string) {
						tokens := strings.Split(path, ".")
						jsonMutators = append(jsonMutators, func(obj map[string]any) {
							_, found, _ := unstructured.NestedString(obj, tokens...)
							if found {
								if err := unstructured.SetNestedField(obj, newValue, tokens...); err != nil {
									t.Fatal(err)
								}
							}
						})
					}

					addSetStringReplacement := func(path string, newValue string) {
						jsonMutators = append(jsonMutators, func(obj map[string]any) {
							if err := setStringAtPath(obj, path, newValue); err != nil {
								t.Fatalf("error from setStringAtPath(%+v): %v", obj, err)
							}
						})
					}

					addReplacement("id", "000000000000000000000")
					addReplacement("uniqueId", "111111111111111111111")
					addReplacement("oauth2ClientId", "888888888888888888888")

					addReplacement("etag", "abcdef0123A=")
					addReplacement("serviceAccount.etag", "abcdef0123A=")
					addReplacement("response.etag", "abcdef0123A=")

					addReplacement("createTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("insertTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("startTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("response.createTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("creationTimestamp", "2024-04-01T12:34:56.123456Z")
					addReplacement("metadata.createTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("metadata.genericMetadata.createTime", "2024-04-01T12:34:56.123456Z")

					addReplacement("updateTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("response.updateTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("metadata.genericMetadata.updateTime", "2024-04-01T12:34:56.123456Z")

					// Specific to spanner
					addReplacement("metadata.startTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("metadata.endTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("metadata.instance.createTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("metadata.instance.updateTime", "2024-04-01T12:34:56.123456Z")

					// Specific to redis
					addReplacement("metadata.createTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("metadata.endTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("response.host", "10.1.2.3")
					addReplacement("response.reservedIpRange", "10.1.2.0/24")
					addReplacement("host", "10.1.2.3")
					addReplacement("reservedIpRange", "10.1.2.0/24")
					addReplacement("metadata.endTime", "2024-04-01T12:34:56.123456Z")

					// Specific to vertexai
					addReplacement("blobStoragePathPrefix", "cloud-ai-platform-00000000-1111-2222-3333-444444444444")
					addReplacement("response.blobStoragePathPrefix", "cloud-ai-platform-00000000-1111-2222-3333-444444444444")
					for _, event := range events {
						responseBody := event.Response.ParseBody()
						if responseBody == nil {
							continue
						}
						metadataArtifact, _, _ := unstructured.NestedString(responseBody, "metadataArtifact")
						if metadataArtifact != "" {
							tokens := strings.Split(metadataArtifact, "/")
							n := len(tokens)
							if n >= 2 {
								kind := tokens[n-2]
								id := tokens[n-1]
								switch kind {
								case "artifacts":
									pathIDs[id] = "${artifactId}"
								}
							}
						}
						gcsBucket, _, _ := unstructured.NestedString(responseBody, "metadata", "gcsBucket")
						if gcsBucket != "" && strings.HasPrefix(gcsBucket, "cloud-ai-platform-") {
							pathIDs[gcsBucket] = "cloud-ai-platform-${bucketId}"
						}
					}

					// Specific to GCS
					addReplacement("timeCreated", "2024-04-01T12:34:56.123456Z")
					addReplacement("updated", "2024-04-01T12:34:56.123456Z")
					addReplacement("softDeletePolicy.effectiveTime", "2024-04-01T12:34:56.123456Z")
					addSetStringReplacement(".acl[].etag", "abcdef0123A=")
					addSetStringReplacement(".defaultObjectAcl[].etag", "abcdef0123A=")

					// Specific to AlloyDB
					addReplacement("uid", "111111111111111111111")
					addReplacement("response.uid", "111111111111111111111")
					addReplacement("continuousBackupInfo.enabledTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("response.continuousBackupInfo.enabledTime", "2024-04-01T12:34:56.123456Z")

					// Specific to BigQuery
					addSetStringReplacement(".access[].userByEmail", "user@google.com")

					// Specific to pubsub
					addReplacement("revisionCreateTime", "2024-04-01T12:34:56.123456Z")
					addReplacement("revisionId", "revision-id-placeholder")

					// Specific to monitoring
					addSetStringReplacement(".creationRecord.mutateTime", "2024-04-01T12:34:56.123456Z")
					addSetStringReplacement(".creationRecord.mutatedBy", "user@example.com")
					addSetStringReplacement(".mutationRecord.mutateTime", "2024-04-01T12:34:56.123456Z")
					addSetStringReplacement(".mutationRecord.mutatedBy", "user@example.com")
					addSetStringReplacement(".mutationRecords[].mutateTime", "2024-04-01T12:34:56.123456Z")
					addSetStringReplacement(".mutationRecords[].mutatedBy", "user@example.com")

					// Replace any empty values in LROs; this is surprisingly difficult to fix in mockgcp
					//
					//     "response": {
					// 	-    "@type": "type.googleapis.com/google.protobuf.Empty"
					// 	+    "@type": "type.googleapis.com/google.protobuf.Empty",
					// 	+    "value": {}
					// 	   }
					jsonMutators = append(jsonMutators, func(obj map[string]any) {
						response := obj["response"]
						if responseMap, ok := response.(map[string]any); ok {
							if responseMap["@type"] == "type.googleapis.com/google.protobuf.Empty" {
								value := responseMap["value"]
								if valueMap, ok := value.(map[string]any); ok && len(valueMap) == 0 {
									delete(responseMap, "value")
								}
							}
						}
					})

					// Remove error details which can contain confidential information
					jsonMutators = append(jsonMutators, func(obj map[string]any) {
						response := obj["error"]
						if responseMap, ok := response.(map[string]any); ok {
							delete(responseMap, "details")
						}
					})
					addReplacement("creationTime", "123456789")
					addReplacement("lastModifiedTime", "123456789")

					events.PrettifyJSON(jsonMutators...)

					// Remove headers that just aren't very relevant to testing
					events.RemoveHTTPResponseHeader("Date")
					events.RemoveHTTPResponseHeader("Alt-Svc")
					events.RemoveHTTPResponseHeader("Server-Timing")
					events.RemoveHTTPResponseHeader("X-Guploader-Uploadid")
					events.RemoveHTTPResponseHeader("Etag")
					events.RemoveHTTPResponseHeader("Content-Length") // an artifact of encoding

					// Replace any expires headers with (rounded) relative offsets
					for _, event := range events {
						expires := event.Response.Header.Get("Expires")
						if expires == "" {
							continue
						}

						if expires == "Mon, 01 Jan 1990 00:00:00 GMT" {
							// Magic value meaning no-cache; don't change
							continue
						}

						expiresTime, err := time.Parse(http.TimeFormat, expires)
						if err != nil {
							t.Fatalf("parsing Expires header %q: %v", expires, err)
						}
						now := time.Now()
						delta := expiresTime.Sub(now)
						if delta > (55 * time.Minute) {
							delta = delta.Round(time.Hour)
							event.Response.Header.Set("Expires", fmt.Sprintf("{now+%vh}", delta.Hours()))
						} else {
							delta = delta.Round(time.Minute)
							event.Response.Header.Set("Expires", fmt.Sprintf("{now+%vm}", delta.Minutes()))
						}
					}

					// Remove repeated GET requests (after normalization)
					{
						var previous *test.LogEntry
						events = events.KeepIf(func(e *test.LogEntry) bool {
							keep := true
							if e.Request.Method == "GET" && previous != nil {
								if previous.Request.Method == "GET" && previous.Request.URL == e.Request.URL {
									if previous.Response.Status == e.Response.Status {
										if previous.Response.Body == e.Response.Body {
											keep = false
										}
									}
								}
							}
							previous = e
							return keep
						})
					}

					got := events.FormatHTTP()
					expectedPath := filepath.Join(fixture.SourceDir, "_http.log")
					normalizers := []func(string) string{}
					normalizers = append(normalizers, IgnoreComments)
					normalizers = append(normalizers, ReplaceString(uniqueID, "${uniqueId}"))
					normalizers = append(normalizers, ReplaceString(project.ProjectID, "${projectId}"))
					normalizers = append(normalizers, ReplaceString(fmt.Sprintf("%d", project.ProjectNumber), "${projectNumber}"))
					if testgcp.TestFolderID.Get() != "" {
						normalizers = append(normalizers, ReplaceString(testgcp.TestFolderID.Get(), "${testFolderId}"))
					}
					for k, v := range pathIDs {
						normalizers = append(normalizers, ReplaceString(k, v))
					}
					for k := range operationIDs {
						normalizers = append(normalizers, ReplaceString(k, "${operationID}"))
					}
					for k := range networkIDs {
						normalizers = append(normalizers, ReplaceString(k, "${networkID}"))
					}

					if testPause {
						assertNoRequest(t, got, normalizers...)
					} else {
						h.CompareGoldenFile(expectedPath, got, normalizers...)
					}
				}
			}
		}
	}
}

// assertNoRequest checks that no POSTs or GETs are made against the cloud provider (GCP). This
// is helpful for when we want to test that Pause works correctly and doesn't actuate resources.
func assertNoRequest(t *testing.T, got string, normalizers ...func(s string) string) {
	t.Helper()

	for _, normalizer := range normalizers {
		got = normalizer(got)
	}

	if strings.Contains(got, "POST") {
		t.Fatalf("unexpected POST in log: %s", got)
	}

	if strings.Contains(got, "GET") {
		t.Fatalf("unexpected GET in log: %s", got)
	}
}

func bytesToUnstructured(t *testing.T, bytes []byte, testID string, project testgcp.GCPProject) *unstructured.Unstructured {
	t.Helper()
	updatedBytes := testcontroller.ReplaceTestVars(t, bytes, testID, project)
	return test.ToUnstructWithNamespace(t, updatedBytes, testID)
}

func createPausedCC(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()

	cc := &opcorev1beta1.ConfigConnector{}
	cc.Spec.Mode = "cluster"
	cc.Spec.Actuation = opcorev1beta1.Paused
	cc.Name = "configconnector.core.cnrm.cloud.google.com"

	if err := c.Create(ctx, cc); err != nil {
		t.Fatalf("error creating CC: %v", err)
	}
}

func verifyKubeWatches(h *create.Harness) {
	// Gather all the watch requests, using the Accept header to determine if it's a metadata-only watch.
	metadataWatches := sets.NewString()
	fullWatches := sets.NewString()
	objectWatches := sets.NewString()
	for _, event := range h.KubeEvents.HTTPEvents {
		if !strings.Contains(event.Request.URL, "watch=true") {
			continue
		}
		u, err := url.Parse(event.Request.URL)
		if err != nil {
			h.Fatalf("cannot parse url %q: %v", event.Request.URL, err)
		}

		metadataWatch := false
		acceptHeader := event.Request.Header.Get("Accept")
		if strings.Contains(acceptHeader, ";as=PartialObjectMetadata") {
			metadataWatch = true
		} else if acceptHeader == "application/json, */*" {
			metadataWatch = false
		} else if acceptHeader == "application/json" {
			metadataWatch = false
		} else if acceptHeader == "application/vnd.kubernetes.protobuf, */*" {
			metadataWatch = false
		} else if acceptHeader == "application/vnd.kubernetes.protobuf" {
			metadataWatch = false
		} else {
			h.Errorf("unhandled Accept header %q", acceptHeader)
		}

		fieldSelector := u.Query().Get("fieldSelector")
		if fieldSelector != "" {
			if strings.HasPrefix(fieldSelector, "metadata.name=") {
				objectName := strings.TrimPrefix(fieldSelector, "metadata.name=")
				objectWatches.Insert(u.Path + "/" + objectName)
				continue
			} else {
				h.Errorf("unhandled fieldSelector %q", fieldSelector)
			}
		}

		if metadataWatch {
			metadataWatches.Insert(u.Path)
		} else {
			fullWatches.Insert(u.Path)
		}
	}

	// Make sure we aren't opening both metadata-only watches and a full watch.
	// If we do this, we will have two caches, we'll get subtle race conditions
	// if we read from both of them.
	for metadataWatch := range metadataWatches {
		if fullWatches.Has(metadataWatch) {
			h.Errorf("two watches on %q (metadata and full); likely to cause race conditions", metadataWatch)
		}
	}

	// Validate the full watches we do have.
	// We only expect full watches on Namespaces, CRDs, CCs and CCCs (currently).
	allowedFullWatches := sets.NewString(
		"/apis/core.cnrm.cloud.google.com/v1beta1/configconnectorcontexts",
		"/apis/core.cnrm.cloud.google.com/v1beta1/configconnectors",
		"/apis/apiextensions.k8s.io/v1/customresourcedefinitions",
	)
	for fullWatch := range fullWatches {
		if !allowedFullWatches.Has(fullWatch) {
			h.Errorf("unexpected full watch on %q", fullWatch)
		}
	}
}

// JSON might be the same, but reordered. Try to sort it before comparing
func sortJSON(s string) (string, error) {
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(s), &data); err != nil {
		return "", err
	}
	sortedJSON, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	return string(sortedJSON), nil
}

func isOperationDone(s string) bool {
	var data map[string]interface{}
	err := json.Unmarshal([]byte(s), &data)
	if err != nil {
		return false
	}
	return data["status"] == "DONE" || data["done"] == true
}

// addTestTimeout will ensure the test fails if not completed before timeout
func addTestTimeout(ctx context.Context, t *testing.T, timeout time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(ctx, timeout)

	done := false
	timedOut := false
	t.Cleanup(func() {
		done = true
		if timedOut {
			t.Fatalf("subtest timeout after %v", timeout)
		}
		cancel()
	})

	go func() {
		<-ctx.Done()
		if !done {
			timedOut = true
		}
	}()

	return ctx
}

func configureVCR(t *testing.T, h *create.Harness) {
	project := h.Project
	replaceWellKnownValues := func(s string) string {
		// Replace project id and number
		result := strings.Replace(s, project.ProjectID, "example-project", -1)
		result = strings.Replace(result, fmt.Sprintf("%d", project.ProjectNumber), "123456789", -1)
		result = strings.Replace(result, os.Getenv("TEST_ORG_ID"), "123450001", -1)

		// Replace user info
		obj := make(map[string]any)
		if err := json.Unmarshal([]byte(s), &obj); err == nil {
			toReplace, _, _ := unstructured.NestedString(obj, "user")
			if len(toReplace) != 0 {
				result = strings.Replace(result, toReplace, "user@google.com", -1)
			}
		}
		return result
	}

	unique := make(map[string]bool)

	hook := func(i *cassette.Interaction) error {
		// Remove internal error message from failed interactions
		resCode := i.Response.Code
		if resCode == 404 || resCode == 400 || resCode == 403 {
			i.Response.Body = "fake error message"
			// Set Content-Length to zero
			i.Response.ContentLength = 0
		}

		// Discard repeated operation retry interactions
		reqURL := i.Request.URL
		resBody := i.Response.Body

		if strings.Contains(reqURL, "operations") {
			if !isOperationDone(resBody) {
				i.DiscardOnSave = true
			}
			sorted, _ := sortJSON(resBody)
			if _, exists := unique[sorted]; !exists {
				unique[sorted] = true // Mark as seen
			} else {
				i.DiscardOnSave = true
			}
		}

		var requestHeadersToRemove = []string{
			"Authorization",
			"User-Agent",
		}
		for _, header := range requestHeadersToRemove {
			delete(i.Request.Headers, header)
		}

		var responseHeadersToRemove = []string{
			"Cache-Control",
			"Server",
			"Vary",
			"X-Content-Type-Options",
			"X-Frame-Options",
			"X-Xss-Protection",
			"Date",
			"Etag",
		}
		for _, header := range responseHeadersToRemove {
			delete(i.Response.Headers, header)
		}

		i.Request.Body = replaceWellKnownValues(i.Request.Body)
		i.Response.Body = replaceWellKnownValues(i.Response.Body)
		i.Request.URL = replaceWellKnownValues(i.Request.URL)

		return nil
	}
	h.VCRRecorderDCL.AddHook(hook, recorder.BeforeSaveHook)
	h.VCRRecorderTF.AddHook(hook, recorder.BeforeSaveHook)
	h.VCRRecorderOauth.AddHook(hook, recorder.BeforeSaveHook)

	matcher := func(r *http.Request, i cassette.Request) bool {
		if r.Method != i.Method || r.URL.String() != i.URL {
			return false
		}

		// Default matcher only checks the request URL and Method. If request body exists, check the body as well.
		// This guarantees that the replayed response matches what the real service would return for that particular request.
		if r.Body != nil && r.Body != http.NoBody {
			var reqBody []byte
			var err error
			reqBody, err = io.ReadAll(r.Body)
			if err != nil {
				t.Fatal("[VCR] Failed to read request body")
			}
			r.Body.Close()
			r.Body = ioutil.NopCloser(bytes.NewBuffer(reqBody))
			if string(reqBody) == i.Body {
				return true
			}

			// If body contains JSON, it might be reordered
			contentType := r.Header.Get("Content-Type")
			if strings.Contains(contentType, "application/json") {
				sortedReqBody, err := sortJSON(string(reqBody))
				if err != nil {
					return false
				}
				sortedBody, err := sortJSON(i.Body)
				if err != nil {
					return false
				}
				return sortedReqBody == sortedBody
			}
		}
		return true
	}
	h.VCRRecorderDCL.SetMatcher(matcher)
	h.VCRRecorderTF.SetMatcher(matcher)
	h.VCRRecorderOauth.SetMatcher(matcher)
}

func containsCCOrCCC(resources []*unstructured.Unstructured) bool {
	for _, resource := range resources {
		gvk := resource.GroupVersionKind()
		switch gvk.GroupKind() {
		case schema.GroupKind{Group: "core.cnrm.cloud.google.com", Kind: "ConfigConnector"},
			schema.GroupKind{Group: "core.cnrm.cloud.google.com", Kind: "ConfigConnectorContext"}:
			return true
		}
	}
	return false
}
