/*
Copyright 2023 The TestGrid Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package resultstore fetches and process results from ResultStore.
package resultstore

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	configpb "github.com/GoogleCloudPlatform/testgrid/pb/config"
	evalpb "github.com/GoogleCloudPlatform/testgrid/pb/custom_evaluator"
	statepb "github.com/GoogleCloudPlatform/testgrid/pb/state"
	teststatuspb "github.com/GoogleCloudPlatform/testgrid/pb/test_status"
	"github.com/GoogleCloudPlatform/testgrid/pkg/updater"
	timestamppb "github.com/golang/protobuf/ptypes/timestamp"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/sirupsen/logrus"
	"google.golang.org/genproto/googleapis/devtools/resultstore/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/testing/protocmp"
)

type fakeClient struct {
	searches    map[string][]string
	invocations map[string]FetchResult
}

func (c *fakeClient) SearchInvocations(ctx context.Context, req *resultstore.SearchInvocationsRequest, opts ...grpc.CallOption) (*resultstore.SearchInvocationsResponse, error) {
	notFound := fmt.Errorf("no results found for %q", req.GetQuery())
	if c.searches == nil {
		return nil, notFound
	}
	invocationIDs, ok := c.searches[req.GetQuery()]
	if !ok {
		return nil, notFound
	}
	var invocations []*resultstore.Invocation
	for _, invocationID := range invocationIDs {
		invoc := &resultstore.Invocation{
			Id: &resultstore.Invocation_Id{InvocationId: invocationID},
		}
		invocations = append(invocations, invoc)
	}
	return &resultstore.SearchInvocationsResponse{Invocations: invocations}, nil
}

func (c *fakeClient) ExportInvocation(ctx context.Context, req *resultstore.ExportInvocationRequest, opts ...grpc.CallOption) (*resultstore.ExportInvocationResponse, error) {
	notFound := fmt.Errorf("no result found for invocation %q", req.GetName())
	if c.invocations == nil {
		return nil, notFound
	}
	result, ok := c.invocations[req.GetName()]
	if !ok {
		return nil, notFound
	}
	return &resultstore.ExportInvocationResponse{
		Invocation:        result.Invocation,
		Actions:           result.Actions,
		ConfiguredTargets: result.ConfiguredTargets,
		Targets:           result.Targets,
	}, nil
}

func invocationName(invocationID string) string {
	return fmt.Sprintf("invocations/%s", invocationID)
}

func targetName(targetID, invocationID string) string {
	return fmt.Sprintf("invocations/%s/targets/%s", invocationID, targetID)
}

func timeMustText(t time.Time) string {
	s, err := t.MarshalText()
	if err != nil {
		panic("timeMustText() panicked")
	}
	return string(s)
}

func TestExtractGroupID(t *testing.T) {
	cases := []struct {
		name string
		tg   *configpb.TestGroup
		pr   *invocation
		want string
	}{
		{
			name: "nil",
		},
		{
			name: "primary grouping BUILD by override config value",
			tg: &configpb.TestGroup{
				DaysOfResults:                   7,
				BuildOverrideConfigurationValue: "test-key-1",
				PrimaryGrouping:                 configpb.TestGroup_PRIMARY_GROUPING_BUILD,
			},
			pr: &invocation{
				InvocationProto: &resultstore.Invocation{
					Id: &resultstore.Invocation_Id{
						InvocationId: "id-1",
					},
					Properties: []*resultstore.Property{
						{
							Key:   "test-key-1",
							Value: "test-val-1",
						},
					},
					Name: invocationName("id-1"),
					Timing: &resultstore.Timing{
						StartTime: &timestamppb.Timestamp{
							Seconds: 1234,
						},
					},
				},
			},
			want: "test-val-1",
		},
		{
			name: "fallback grouping BUILD resort to default",
			tg: &configpb.TestGroup{
				DaysOfResults:                   7,
				BuildOverrideConfigurationValue: "test-key-1",
				FallbackGrouping:                configpb.TestGroup_FALLBACK_GROUPING_BUILD,
			},
			pr: &invocation{
				InvocationProto: &resultstore.Invocation{
					Id: &resultstore.Invocation_Id{
						InvocationId: "id-1",
					},
					Properties: []*resultstore.Property{
						{
							Key:   "test-key-1",
							Value: "test-val-1",
						},
					},
					Name: invocationName("id-1"),
					Timing: &resultstore.Timing{
						StartTime: &timestamppb.Timestamp{
							Seconds: 1234,
						},
					},
				},
			},
			want: "id-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractGroupID(tc.tg, tc.pr)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("extractGroupID() differed (-want, +got): %s", diff)
			}
		})
	}
}

func TestColumnReader(t *testing.T) {
	// We already have functions testing 'stop' logic.
	// Scope this test to whether the column reader fetches and returns ascending results.
	oneMonthConfig := &configpb.TestGroup{
		Name:          "a-test-group",
		DaysOfResults: 30,
	}
	now := time.Now()
	oneDayAgo := now.AddDate(0, 0, -1)
	twoDaysAgo := now.AddDate(0, 0, -2)
	threeDaysAgo := now.AddDate(0, 0, -3)
	oneMonthAgo := now.AddDate(0, 0, -30)
	testQueryAfter := queryAfter(queryProw, oneMonthAgo)
	cases := []struct {
		name    string
		client  *fakeClient
		tg      *configpb.TestGroup
		want    []updater.InflatedColumn
		wantErr bool
	}{
		{
			name:    "empty",
			tg:      oneMonthConfig,
			wantErr: true,
		},
		{
			name: "basic",
			tg: &configpb.TestGroup{
				DaysOfResults: 30,
			},
			client: &fakeClient{
				searches: map[string][]string{
					testQueryAfter: {"id-1", "id-2"},
				},
				invocations: map[string]FetchResult{
					invocationName("id-1"): {
						Invocation: &resultstore.Invocation{
							Id: &resultstore.Invocation_Id{
								InvocationId: "id-1",
							},
							Name: invocationName("id-1"),
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: oneDayAgo.Unix(),
								},
							},
						},
						Targets: []*resultstore.Target{
							{
								Id: &resultstore.Target_Id{
									TargetId: "tgt-id-1",
								},
								StatusAttributes: &resultstore.StatusAttributes{
									Status: resultstore.Status_PASSED,
								},
							},
						},
						ConfiguredTargets: []*resultstore.ConfiguredTarget{
							{
								Id: &resultstore.ConfiguredTarget_Id{
									TargetId: "tgt-id-1",
								},
								StatusAttributes: &resultstore.StatusAttributes{
									Status: resultstore.Status_PASSED,
								},
							},
						},
						Actions: []*resultstore.Action{
							{
								Id: &resultstore.Action_Id{
									TargetId: "tgt-id-1",
									ActionId: "build",
								},
							},
						},
					},
					invocationName("id-2"): {
						Invocation: &resultstore.Invocation{
							Id: &resultstore.Invocation_Id{
								InvocationId: "id-2",
							},
							Name: invocationName("id-2"),
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: twoDaysAgo.Unix(),
								},
							},
						},
						Targets: []*resultstore.Target{
							{
								Id: &resultstore.Target_Id{
									TargetId: "tgt-id-1",
								},
								StatusAttributes: &resultstore.StatusAttributes{
									Status: resultstore.Status_FAILED,
								},
							},
						},
						ConfiguredTargets: []*resultstore.ConfiguredTarget{
							{
								Id: &resultstore.ConfiguredTarget_Id{
									TargetId: "tgt-id-1",
								},
								StatusAttributes: &resultstore.StatusAttributes{
									Status: resultstore.Status_FAILED,
								},
							},
						},
						Actions: []*resultstore.Action{
							{
								Id: &resultstore.Action_Id{
									TargetId: "tgt-id-1",
									ActionId: "build",
								},
							},
						},
					},
				},
			},
			want: []updater.InflatedColumn{
				{
					Column: &statepb.Column{
						Build:   "id-1",
						Name:    "id-1",
						Started: float64(oneDayAgo.Unix() * 1000),
						Hint:    timeMustText(oneDayAgo.Local().Truncate(time.Second)),
					},
					Cells: map[string]updater.Cell{
						"tgt-id-1": {
							ID:     "tgt-id-1",
							CellID: "id-1",
							Result: teststatuspb.TestStatus_PASS,
						},
					},
				},
				{
					Column: &statepb.Column{
						Build:   "id-2",
						Name:    "id-2",
						Started: float64(twoDaysAgo.Unix() * 1000),
						Hint:    timeMustText(twoDaysAgo.Truncate(time.Second)),
					},
					Cells: map[string]updater.Cell{
						"tgt-id-1": {
							ID:     "tgt-id-1",
							CellID: "id-2",
							Result: teststatuspb.TestStatus_FAIL,
						},
					},
				},
			},
		},
		{
			name: "no results from query",
			tg:   oneMonthConfig,
			client: &fakeClient{
				searches: map[string][]string{},
				invocations: map[string]FetchResult{
					invocationName("id-1"): {
						Invocation: &resultstore.Invocation{
							Id: &resultstore.Invocation_Id{
								InvocationId: "id-1",
							},
							Name: invocationName("id-1"),
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: oneDayAgo.Unix(),
								},
							},
						},
					},
					invocationName("id-2"): {
						Invocation: &resultstore.Invocation{
							Id: &resultstore.Invocation_Id{
								InvocationId: "id-2",
							},
							Name: invocationName("id-2"),
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: twoDaysAgo.Unix(),
								},
							},
						},
					},
					invocationName("id-3"): {
						Invocation: &resultstore.Invocation{
							Id: &resultstore.Invocation_Id{
								InvocationId: "id-3",
							},
							Name: invocationName("id-3"),
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: threeDaysAgo.Unix(),
								},
							},
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "no invocations found",
			client: &fakeClient{
				searches: map[string][]string{
					testQueryAfter: {"id-2", "id-3", "id-1"},
				},
				invocations: map[string]FetchResult{},
			},
			want: nil,
		},
		{
			name: "ids not in order",
			client: &fakeClient{
				searches: map[string][]string{
					testQueryAfter: {"id-2", "id-3", "id-1"},
				},
				invocations: map[string]FetchResult{
					invocationName("id-1"): {
						Invocation: &resultstore.Invocation{
							Id: &resultstore.Invocation_Id{
								InvocationId: "id-1",
							},
							Name: invocationName("id-1"),
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: oneDayAgo.Unix(),
								},
							},
						},
					},
					invocationName("id-2"): {
						Invocation: &resultstore.Invocation{
							Id: &resultstore.Invocation_Id{
								InvocationId: "id-2",
							},
							Name: invocationName("id-2"),
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: twoDaysAgo.Unix(),
								},
							},
						},
					},
					invocationName("id-3"): {
						Invocation: &resultstore.Invocation{
							Id: &resultstore.Invocation_Id{
								InvocationId: "id-3",
							},
							Name: invocationName("id-3"),
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: threeDaysAgo.Unix(),
								},
							},
						},
					},
				},
			},
			want: []updater.InflatedColumn{
				{
					Column: &statepb.Column{
						Build:   "id-1",
						Name:    "id-1",
						Started: float64(oneDayAgo.Unix() * 1000),
						Hint:    timeMustText(oneDayAgo.Truncate(time.Second)),
					},
					Cells: map[string]updater.Cell{},
				},
				{
					Column: &statepb.Column{
						Build:   "id-2",
						Name:    "id-2",
						Started: float64(twoDaysAgo.Unix() * 1000),
						Hint:    timeMustText(twoDaysAgo.Truncate(time.Second)),
					},
					Cells: map[string]updater.Cell{},
				},
				{
					Column: &statepb.Column{
						Build:   "id-3",
						Name:    "id-3",
						Started: float64(threeDaysAgo.Unix() * 1000),
						Hint:    timeMustText(threeDaysAgo.Truncate(time.Second)),
					},
					Cells: map[string]updater.Cell{},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dlClient *DownloadClient
			if tc.client != nil {
				dlClient = &DownloadClient{client: tc.client}
			}
			columnReader := ColumnReader(dlClient, 0)
			var got []updater.InflatedColumn
			ch := make(chan updater.InflatedColumn)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for col := range ch {
					got = append(got, col)
				}
			}()
			err := columnReader(context.Background(), logrus.WithField("case", tc.name), oneMonthConfig, nil, oneMonthAgo, ch)
			close(ch)
			wg.Wait()
			if err != nil && !tc.wantErr {
				t.Errorf("columnReader() errored: %v", err)
			} else if err == nil && tc.wantErr {
				t.Errorf("columnReader() did not error as expected")
			}
			if diff := cmp.Diff(tc.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("columnReader() differed (-want, +got): %s", diff)
			}
		})
	}
}

func TestCellMessageIcon(t *testing.T) {
	cases := []struct {
		name        string
		annotations []*configpb.TestGroup_TestAnnotation
		properties  map[string][]string
		tags        []string
		message     string
		icon        string
	}{
		{
			name: "basically works",
		},
		{
			name: "find annotation from property",
			annotations: []*configpb.TestGroup_TestAnnotation{
				{
					ShortText: "icon",
					ShortTextMessageSource: &configpb.TestGroup_TestAnnotation_PropertyName{
						PropertyName: "props",
					},
				},
			},
			properties: map[string][]string{
				"props": {"first", "second"},
			},
			message: "first",
			icon:    "icon",
		},
		{
			name: "find annotation from tag",
			annotations: []*configpb.TestGroup_TestAnnotation{
				{
					ShortText: "icon",
					ShortTextMessageSource: &configpb.TestGroup_TestAnnotation_PropertyName{
						PropertyName: "actually-a-tag",
					},
				},
			},
			tags:    []string{"actually-a-tag"},
			message: "actually-a-tag",
			icon:    "icon",
		},
		{
			name: "find annotation from tag",
			annotations: []*configpb.TestGroup_TestAnnotation{
				{
					ShortText: "icon",
					ShortTextMessageSource: &configpb.TestGroup_TestAnnotation_PropertyName{
						PropertyName: "actually-a-tag",
					},
				},
				{
					ShortText: "icon-2",
					ShortTextMessageSource: &configpb.TestGroup_TestAnnotation_PropertyName{
						PropertyName: "actually-a-tag-2",
					},
				},
			},
			tags:    []string{"actually-a-tag"},
			message: "actually-a-tag",
			icon:    "icon",
		},
	}

	for _, tc := range cases {
		message, icon := cellMessageIcon(tc.annotations, tc.properties, tc.tags)
		if tc.message != message {
			t.Errorf("cellMessageIcon() got unexpected message %q, want %q", message, tc.message)
		}
		if tc.icon != icon {
			t.Errorf("cellMessageIcon() got unexpected icon %q, want %q", icon, tc.icon)
		}
	}
}

func TestTimestampMilliseconds(t *testing.T) {
	cases := []struct {
		name      string
		timestamp *timestamppb.Timestamp
		want      float64
	}{
		{
			name:      "nil",
			timestamp: nil,
			want:      0,
		},
		{
			name:      "zero",
			timestamp: &timestamppb.Timestamp{},
			want:      0,
		},
		{
			name: "basic",
			timestamp: &timestamppb.Timestamp{
				Seconds: 1234,
				Nanos:   5678,
			},
			want: 1234005.678,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := timestampMilliseconds(tc.timestamp)
			approx := cmpopts.EquateApprox(.01, 0)
			if diff := cmp.Diff(tc.want, got, approx); diff != "" {
				t.Errorf("timestampMilliseconds(%v) differed (-want, +got): %s", tc.timestamp, diff)
			}
		})
	}
}

func TestProcessRawResult(t *testing.T) {
	cases := []struct {
		name   string
		result *FetchResult
		want   *invocation
	}{
		{
			name: "just invocation",
			result: &FetchResult{
				Invocation: &resultstore.Invocation{
					Name: invocationName("Best invocation"),
					Id: &resultstore.Invocation_Id{
						InvocationId: "uuid-222",
					},
				},
			},
			want: &invocation{
				InvocationProto: &resultstore.Invocation{
					Name: invocationName("Best invocation"),
					Id: &resultstore.Invocation_Id{
						InvocationId: "uuid-222",
					},
				},
				TargetResults: make(map[string][]*singleActionResult),
			},
		},
		{
			name: "invocation + targets + configured targets",
			result: &FetchResult{
				Invocation: &resultstore.Invocation{
					Name: invocationName("Best invocation"),
					Id: &resultstore.Invocation_Id{
						InvocationId: "uuid-222",
					},
				},
				Targets: []*resultstore.Target{
					{
						Name: targetName("updater", "uuid-222"),
						Id: &resultstore.Target_Id{
							InvocationId: "uuid-222",
							TargetId:     "tgt-uuid-1",
						},
					},
					{
						Name: targetName("tabulator", "uuid-222"),
						Id: &resultstore.Target_Id{
							InvocationId: "uuid-222",
							TargetId:     "tgt-uuid-2",
						},
					},
				},
				ConfiguredTargets: []*resultstore.ConfiguredTarget{
					{
						Name: targetName("updater", "uuid-222"),
						Id: &resultstore.ConfiguredTarget_Id{
							InvocationId: "uuid-222",
							TargetId:     "tgt-uuid-1",
						},
					},
					{
						Name: targetName("tabulator", "uuid-222"),
						Id: &resultstore.ConfiguredTarget_Id{
							InvocationId: "uuid-222",
							TargetId:     "tgt-uuid-2",
						},
					},
				},
			},
			want: &invocation{
				InvocationProto: &resultstore.Invocation{
					Name: invocationName("Best invocation"),
					Id: &resultstore.Invocation_Id{
						InvocationId: "uuid-222",
					},
				},
				TargetResults: map[string][]*singleActionResult{
					"tgt-uuid-1": {
						{
							TargetProto: &resultstore.Target{
								Name: targetName("updater", "uuid-222"),
								Id: &resultstore.Target_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-1",
								},
							},
							ConfiguredTargetProto: &resultstore.ConfiguredTarget{
								Name: targetName("updater", "uuid-222"),
								Id: &resultstore.ConfiguredTarget_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-1",
								},
							},
						},
					},
					"tgt-uuid-2": {
						{
							TargetProto: &resultstore.Target{
								Name: targetName("tabulator", "uuid-222"),
								Id: &resultstore.Target_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-2",
								},
							},
							ConfiguredTargetProto: &resultstore.ConfiguredTarget{
								Name: targetName("tabulator", "uuid-222"),
								Id: &resultstore.ConfiguredTarget_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-2",
								},
							},
						},
					},
				},
			},
		},
		{
			name: "all together + extra actions",
			result: &FetchResult{
				Invocation: &resultstore.Invocation{
					Name: invocationName("Best invocation"),
					Id: &resultstore.Invocation_Id{
						InvocationId: "uuid-222",
					},
				},
				Targets: []*resultstore.Target{
					{
						Name: "/testgrid/backend:updater",
						Id: &resultstore.Target_Id{
							InvocationId: "uuid-222",
							TargetId:     "tgt-uuid-1",
						},
					},
					{
						Name: "/testgrid/backend:tabulator",
						Id: &resultstore.Target_Id{
							InvocationId: "uuid-222",
							TargetId:     "tgt-uuid-2",
						},
					},
				},
				ConfiguredTargets: []*resultstore.ConfiguredTarget{
					{
						Name: "/testgrid/backend:updater",
						Id: &resultstore.ConfiguredTarget_Id{
							InvocationId: "uuid-222",
							TargetId:     "tgt-uuid-1",
						},
					},
					{
						Name: "/testgrid/backend:tabulator",
						Id: &resultstore.ConfiguredTarget_Id{
							InvocationId: "uuid-222",
							TargetId:     "tgt-uuid-2",
						},
					},
				},
				Actions: []*resultstore.Action{
					{
						Name: "/testgrid/backend:updater",
						Id: &resultstore.Action_Id{
							InvocationId: "uuid-222",
							TargetId:     "tgt-uuid-1",
							ActionId:     "flying",
						},
					},
					{
						Name: "/testgrid/backend:tabulator",
						Id: &resultstore.Action_Id{
							InvocationId: "uuid-222",
							TargetId:     "tgt-uuid-2",
							ActionId:     "walking",
						},
					},
					{
						Name: "/testgrid/backend:tabulator",
						Id: &resultstore.Action_Id{
							InvocationId: "uuid-222",
							TargetId:     "tgt-uuid-2",
							ActionId:     "flying",
						},
					},
				},
			},
			want: &invocation{
				InvocationProto: &resultstore.Invocation{
					Name: invocationName("Best invocation"),
					Id: &resultstore.Invocation_Id{
						InvocationId: "uuid-222",
					},
				},
				TargetResults: map[string][]*singleActionResult{
					"tgt-uuid-1": {
						{
							TargetProto: &resultstore.Target{
								Name: "/testgrid/backend:updater",
								Id: &resultstore.Target_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-1",
								},
							},
							ConfiguredTargetProto: &resultstore.ConfiguredTarget{
								Name: "/testgrid/backend:updater",
								Id: &resultstore.ConfiguredTarget_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-1",
								},
							},
							ActionProto: &resultstore.Action{
								Name: "/testgrid/backend:updater",
								Id: &resultstore.Action_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-1",
									ActionId:     "flying",
								},
							},
						},
					},
					"tgt-uuid-2": {
						{
							TargetProto: &resultstore.Target{
								Name: "/testgrid/backend:tabulator",
								Id: &resultstore.Target_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-2",
								},
							},
							ConfiguredTargetProto: &resultstore.ConfiguredTarget{
								Name: "/testgrid/backend:tabulator",
								Id: &resultstore.ConfiguredTarget_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-2",
								},
							},
							ActionProto: &resultstore.Action{
								Name: "/testgrid/backend:tabulator",
								Id: &resultstore.Action_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-2",
									ActionId:     "walking",
								},
							},
						}, {
							TargetProto: &resultstore.Target{
								Name: "/testgrid/backend:tabulator",
								Id: &resultstore.Target_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-2",
								},
							},
							ConfiguredTargetProto: &resultstore.ConfiguredTarget{
								Name: "/testgrid/backend:tabulator",
								Id: &resultstore.ConfiguredTarget_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-2",
								},
							},
							ActionProto: &resultstore.Action{
								Name: "/testgrid/backend:tabulator",
								Id: &resultstore.Action_Id{
									InvocationId: "uuid-222",
									TargetId:     "tgt-uuid-2",
									ActionId:     "flying",
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := processRawResult(logrus.WithField("case", tc.name), tc.result)
			if diff := cmp.Diff(tc.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("processRawResult(...) differed (-want, +got): %s", diff)
			}
		})
	}

}
func TestProcessGroup(t *testing.T) {

	cases := []struct {
		name  string
		tg    *configpb.TestGroup
		group *invocationGroup
		want  *updater.InflatedColumn
	}{
		{
			name: "nil",
			want: nil,
		},
		{
			name:  "empty",
			group: &invocationGroup{},
			want:  nil,
		},
		{
			name: "basic invocation group",
			group: &invocationGroup{
				GroupID: "uuid-123",
				Invocations: []*invocation{
					{
						InvocationProto: &resultstore.Invocation{
							Name: invocationName("uuid-123"),
							Id: &resultstore.Invocation_Id{
								InvocationId: "uuid-123",
							},
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: 1234,
								},
							},
						},
						TargetResults: map[string][]*singleActionResult{
							"tgt-id-1": {
								{
									ConfiguredTargetProto: &resultstore.ConfiguredTarget{
										Id: &resultstore.ConfiguredTarget_Id{
											TargetId: "tgt-id-1",
										},
										StatusAttributes: &resultstore.StatusAttributes{
											Status: resultstore.Status_PASSED,
										},
									},
								},
							},
							"tgt-id-2": {
								{
									ConfiguredTargetProto: &resultstore.ConfiguredTarget{
										Id: &resultstore.ConfiguredTarget_Id{
											TargetId: "tgt-id-2",
										},
										StatusAttributes: &resultstore.StatusAttributes{
											Status: resultstore.Status_FAILED,
										},
									},
								},
							},
						},
					},
				},
			},
			want: &updater.InflatedColumn{
				Column: &statepb.Column{
					Name:    "uuid-123",
					Build:   "uuid-123",
					Started: 1234000,
					Hint:    "1970-01-01T00:20:34Z",
				},
				Cells: map[string]updater.Cell{
					"tgt-id-1": {
						ID:     "tgt-id-1",
						CellID: "uuid-123",
						Result: teststatuspb.TestStatus_PASS,
					},
					"tgt-id-2": {
						ID:     "tgt-id-2",
						CellID: "uuid-123",
						Result: teststatuspb.TestStatus_FAIL,
					},
				},
			},
		},
		{
			name: "advanced invocation group with several invocations and repeated targets",
			tg: &configpb.TestGroup{
				BuildOverrideConfigurationValue: "pi-key-chu",
			},
			group: &invocationGroup{
				GroupID: "snorlax",
				Invocations: []*invocation{
					{
						InvocationProto: &resultstore.Invocation{
							Name: invocationName("uuid-123"),
							Id: &resultstore.Invocation_Id{
								InvocationId: "uuid-123",
							},
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: 1234,
								},
							},
							Properties: []*resultstore.Property{
								{
									Key:   "pi-key-chu",
									Value: "snorlax",
								},
							},
						},
						TargetResults: map[string][]*singleActionResult{
							"tgt-id-1": {
								{
									ConfiguredTargetProto: &resultstore.ConfiguredTarget{
										Id: &resultstore.ConfiguredTarget_Id{
											TargetId: "tgt-id-1",
										},
										StatusAttributes: &resultstore.StatusAttributes{
											Status: resultstore.Status_PASSED,
										},
									},
								},
							},
							"tgt-id-2": {
								{
									ConfiguredTargetProto: &resultstore.ConfiguredTarget{
										Id: &resultstore.ConfiguredTarget_Id{
											TargetId: "tgt-id-2",
										},
										StatusAttributes: &resultstore.StatusAttributes{
											Status: resultstore.Status_FAILED,
										},
									},
								},
							},
						},
					},
					{
						InvocationProto: &resultstore.Invocation{
							Name: invocationName("uuid-124"),
							Id: &resultstore.Invocation_Id{
								InvocationId: "uuid-124",
							},
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: 1334,
								},
							},
							Properties: []*resultstore.Property{
								{
									Key:   "pi-key-chu",
									Value: "snorlax",
								},
							},
						},
						TargetResults: map[string][]*singleActionResult{
							"tgt-id-1": {
								{
									ConfiguredTargetProto: &resultstore.ConfiguredTarget{
										Id: &resultstore.ConfiguredTarget_Id{
											TargetId: "tgt-id-1",
										},
										StatusAttributes: &resultstore.StatusAttributes{
											Status: resultstore.Status_PASSED,
										},
									},
								},
							},
							"tgt-id-2": {
								{
									ConfiguredTargetProto: &resultstore.ConfiguredTarget{
										Id: &resultstore.ConfiguredTarget_Id{
											TargetId: "tgt-id-2",
										},
										StatusAttributes: &resultstore.StatusAttributes{
											Status: resultstore.Status_FAILED,
										},
									},
								},
							},
						},
					},
				},
			},
			want: &updater.InflatedColumn{
				Column: &statepb.Column{
					Name:    "snorlax",
					Build:   "snorlax",
					Started: 1234000,
					Hint:    "1970-01-01T00:22:14Z",
				},
				Cells: map[string]updater.Cell{
					"tgt-id-1": {
						ID:     "tgt-id-1",
						CellID: "uuid-123",
						Result: teststatuspb.TestStatus_PASS,
					},
					"tgt-id-2": {
						ID:     "tgt-id-2",
						CellID: "uuid-123",
						Result: teststatuspb.TestStatus_FAIL,
					},
					"tgt-id-1 [1]": {
						ID:     "tgt-id-1",
						CellID: "uuid-124",
						Result: teststatuspb.TestStatus_PASS,
					},
					"tgt-id-2 [1]": {
						ID:     "tgt-id-2",
						CellID: "uuid-124",
						Result: teststatuspb.TestStatus_FAIL,
					},
				},
			},
		},
		{
			name: "invocation group with single invocation and disabled test result",
			tg: &configpb.TestGroup{
				BuildOverrideConfigurationValue: "pi-key-chu",
				MaxTestMethodsPerTest:           10,
				EnableTestMethods:               true,
			},
			group: &invocationGroup{
				GroupID: "snorlax",
				Invocations: []*invocation{
					{
						InvocationProto: &resultstore.Invocation{
							Name: invocationName("uuid-123"),
							Id: &resultstore.Invocation_Id{
								InvocationId: "uuid-123",
							},
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: 1234,
								},
							},
							Properties: []*resultstore.Property{
								{
									Key:   "pi-key-chu",
									Value: "snorlax",
								},
							},
						},
						TargetResults: map[string][]*singleActionResult{
							"tgt-id-1": {
								{
									ConfiguredTargetProto: &resultstore.ConfiguredTarget{
										Id: &resultstore.ConfiguredTarget_Id{
											TargetId: "tgt-id-1",
										},
										StatusAttributes: &resultstore.StatusAttributes{
											Status: resultstore.Status_PASSED,
										},
									},
									ActionProto: &resultstore.Action{
										ActionType: &resultstore.Action_TestAction{
											TestAction: &resultstore.TestAction{
												TestSuite: &resultstore.TestSuite{
													SuiteName: "TestDetectJSError",
													Tests: []*resultstore.Test{
														{
															TestType: &resultstore.Test_TestCase{
																TestCase: &resultstore.TestCase{
																	CaseName: "DISABLED_case",
																	Result:   resultstore.TestCase_SKIPPED,
																},
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			want: &updater.InflatedColumn{
				Column: &statepb.Column{
					Name:    "snorlax",
					Build:   "snorlax",
					Started: 1234000,
					Hint:    "1970-01-01T00:20:34Z",
				},
				Cells: map[string]updater.Cell{
					"tgt-id-1": {
						ID:     "tgt-id-1",
						CellID: "uuid-123",
						Result: teststatuspb.TestStatus_PASS,
					},
					"tgt-id-1@TESTGRID@DISABLED_case": {
						Result: teststatuspb.TestStatus_PASS_WITH_SKIPS,
						ID:     "tgt-id-1",
						CellID: "uuid-123"},
				},
			},
		},
		{
			name: "invocation group with single invocation and test result with failure and error",
			tg: &configpb.TestGroup{
				BuildOverrideConfigurationValue: "pi-key-chu",
				MaxTestMethodsPerTest:           10,
				EnableTestMethods:               true,
			},
			group: &invocationGroup{
				GroupID: "snorlax",
				Invocations: []*invocation{
					{
						InvocationProto: &resultstore.Invocation{
							Name: invocationName("uuid-123"),
							Id: &resultstore.Invocation_Id{
								InvocationId: "uuid-123",
							},
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: 1234,
								},
							},
							Properties: []*resultstore.Property{
								{
									Key:   "pi-key-chu",
									Value: "snorlax",
								},
							},
						},
						TargetResults: map[string][]*singleActionResult{
							"tgt-id-1": {
								{
									ConfiguredTargetProto: &resultstore.ConfiguredTarget{
										Id: &resultstore.ConfiguredTarget_Id{
											TargetId: "tgt-id-1",
										},
										StatusAttributes: &resultstore.StatusAttributes{
											Status: resultstore.Status_PASSED,
										},
									},
									ActionProto: &resultstore.Action{
										ActionType: &resultstore.Action_TestAction{
											TestAction: &resultstore.TestAction{
												TestSuite: &resultstore.TestSuite{
													SuiteName: "TestDetectJSError",
													Tests: []*resultstore.Test{
														{
															TestType: &resultstore.Test_TestCase{
																TestCase: &resultstore.TestCase{
																	CaseName: "Not_working_case",
																	Failures: []*resultstore.TestFailure{
																		{
																			FailureMessage: "foo",
																		},
																	},
																	Errors: []*resultstore.TestError{
																		{
																			ErrorMessage: "bar",
																		},
																	},
																},
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			want: &updater.InflatedColumn{
				Column: &statepb.Column{
					Name:    "snorlax",
					Build:   "snorlax",
					Started: 1234000,
					Hint:    "1970-01-01T00:20:34Z",
				},
				Cells: map[string]updater.Cell{
					"tgt-id-1": {
						ID:     "tgt-id-1",
						CellID: "uuid-123",
						Result: teststatuspb.TestStatus_PASS,
					},
					"tgt-id-1@TESTGRID@Not_working_case": {
						Result: teststatuspb.TestStatus_FAIL,
						ID:     "tgt-id-1",
						CellID: "uuid-123"},
				},
			},
		},
		{
			name: "invocation group with ignored statuses and custom target status evaluator",
			tg: &configpb.TestGroup{
				IgnorePending: true,
				CustomEvaluatorRuleSet: &evalpb.RuleSet{
					Rules: []*evalpb.Rule{
						{
							ComputedStatus: teststatuspb.TestStatus_CATEGORIZED_ABORT,
							TestResultComparisons: []*evalpb.TestResultComparison{
								{
									TestResultInfo: &evalpb.TestResultComparison_TargetStatus{
										TargetStatus: true,
									},
									Comparison: &evalpb.Comparison{
										Op: evalpb.Comparison_OP_EQ,
										ComparisonValue: &evalpb.Comparison_TargetStatusValue{
											TargetStatusValue: teststatuspb.TestStatus_TIMED_OUT,
										},
									},
								},
							},
						},
					},
				},
			},
			group: &invocationGroup{
				GroupID: "uuid-123",
				Invocations: []*invocation{
					{
						InvocationProto: &resultstore.Invocation{
							Name: invocationName("uuid-123"),
							Id: &resultstore.Invocation_Id{
								InvocationId: "uuid-123",
							},
							Timing: &resultstore.Timing{
								StartTime: &timestamppb.Timestamp{
									Seconds: 1234,
								},
							},
						},
						TargetResults: map[string][]*singleActionResult{
							"tgt-id-1": {
								{
									ConfiguredTargetProto: &resultstore.ConfiguredTarget{
										Id: &resultstore.ConfiguredTarget_Id{
											TargetId: "tgt-id-1",
										},
										StatusAttributes: &resultstore.StatusAttributes{
											Status: resultstore.Status_PASSED,
										},
									},
								},
							},
							"tgt-id-2": {
								{
									ConfiguredTargetProto: &resultstore.ConfiguredTarget{
										Id: &resultstore.ConfiguredTarget_Id{
											TargetId: "tgt-id-2",
										},
										StatusAttributes: &resultstore.StatusAttributes{
											Status: resultstore.Status_TESTING,
										},
									},
								},
							},
							"tgt-id-3": {
								{
									ConfiguredTargetProto: &resultstore.ConfiguredTarget{
										Id: &resultstore.ConfiguredTarget_Id{
											TargetId: "tgt-id-3",
										},
										StatusAttributes: &resultstore.StatusAttributes{
											Status: resultstore.Status_TIMED_OUT,
										},
									},
								},
							},
						},
					},
				},
			},
			want: &updater.InflatedColumn{
				Column: &statepb.Column{
					Name:    "uuid-123",
					Build:   "uuid-123",
					Started: 1234000,
					Hint:    "1970-01-01T00:20:34Z",
				},
				Cells: map[string]updater.Cell{
					"tgt-id-1": {
						ID:     "tgt-id-1",
						CellID: "uuid-123",
						Result: teststatuspb.TestStatus_PASS,
					},
					"tgt-id-3": {
						ID:     "tgt-id-3",
						CellID: "uuid-123",
						Result: teststatuspb.TestStatus_CATEGORIZED_ABORT,
					},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := processGroup(tc.tg, tc.group)
			if diff := cmp.Diff(tc.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("processGroup() differed (-want, +got): %s", diff)
			}
		})
	}
}

func TestFilterResults(t *testing.T) {
	cases := []struct {
		name       string
		results    []*resultstore.Test
		properties []*configpb.TestGroup_KeyValue
		match      *string
		unmatch    *string
		expected   []*resultstore.Test
		filtered   bool
	}{
		{
			name: "basically works",
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "every",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "thing",
						},
					},
				},
			},
			expected: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "every",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "thing",
						},
					},
				},
			},
		},
		{
			name: "match nothing",
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "every",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "thing",
						},
					},
				},
			},
			match:    pstr("sandwiches"),
			filtered: true,
		},
		{
			name: "all wrong properties",
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "every",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "thing",
						},
					},
				},
			},
			properties: []*configpb.TestGroup_KeyValue{
				{
					Key:   "medal",
					Value: "gold",
				},
			},
			filtered: true,
		},
		{
			name:       "properties flter",
			properties: []*configpb.TestGroup_KeyValue{{}},
			filtered:   true,
		},
		{
			name:     "match filters",
			match:    pstr(".*"),
			filtered: true,
		},
		{
			name:     "unmatch filters",
			match:    pstr("^$"),
			filtered: true,
		},
		{
			name:    "match fruit",
			match:   pstr("tomato|apple|orange"),
			unmatch: pstr("tomato"),
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "steak",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "tomato",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "apple",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "orange",
						},
					},
				},
			},
			expected: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "apple",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "orange",
						},
					},
				},
			},
			filtered: true,
		},
		{
			name: "good properties",
			properties: []*configpb.TestGroup_KeyValue{
				{
					Key:   "tastes",
					Value: "good",
				},
			},
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "potion",
							Properties: []*resultstore.Property{
								{
									Key:   "tastes",
									Value: "bad",
								},
							},
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "fruit",
							Properties: []*resultstore.Property{
								{
									Key:   "tastes",
									Value: "good",
								},
							},
						},
					},
				},
			},
			expected: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "fruit",
							Properties: []*resultstore.Property{
								{
									Key:   "tastes",
									Value: "good",
								},
							},
						},
					},
				},
			},
			filtered: true,
		},
		{
			name: "both filter",
			properties: []*configpb.TestGroup_KeyValue{
				{
					Key:   "tastes",
					Value: "good",
				},
			},
			unmatch: pstr("steak"),
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "potion",
							Properties: []*resultstore.Property{
								{
									Key:   "tastes",
									Value: "bad",
								},
							},
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "fruit",
							Properties: []*resultstore.Property{
								{
									Key:   "tastes",
									Value: "good",
								},
							},
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "steak",
							Properties: []*resultstore.Property{
								{
									Key:   "tastes",
									Value: "good",
								},
							},
						},
					},
				},
			},
			expected: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "fruit",
							Properties: []*resultstore.Property{
								{
									Key:   "tastes",
									Value: "good",
								},
							},
						},
					},
				},
			},
			filtered: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var match, unmatch *regexp.Regexp
			if tc.match != nil {
				match = regexp.MustCompile(*tc.match)
			}
			if tc.unmatch != nil {
				unmatch = regexp.MustCompile(*tc.unmatch)
			}
			actual, filtered := filterResults(tc.results, tc.properties, match, unmatch)
			if diff := cmp.Diff(actual, tc.expected, protocmp.Transform()); diff != "" {
				t.Errorf("filterResults() got unexpected diff (-have, +want):\n%s", diff)
			}
			if filtered != tc.filtered {
				t.Errorf("filterResults() got filtered %t, want %t", filtered, tc.filtered)
			}
		})
	}
}

func TestFilterProperties(t *testing.T) {
	cases := []struct {
		name       string
		results    []*resultstore.Test
		properties []*configpb.TestGroup_KeyValue
		expected   []*resultstore.Test
	}{
		{
			name: "basically works",
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "every",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "every",
						},
					},
				},
			},
			expected: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "every",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "every",
						},
					},
				},
			},
		},
		{
			name: "must have correct key and value",
			properties: []*configpb.TestGroup_KeyValue{
				{
					Key:   "goal",
					Value: "gold",
				},
			},
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "wrong-key",
							Properties: []*resultstore.Property{
								{
									Key:   "random",
									Value: "gold",
								},
							},
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "correct-key",
							Properties: []*resultstore.Property{
								{
									Key:   "goal",
									Value: "gold",
								},
							},
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "wrong-value",
							Properties: []*resultstore.Property{
								{
									Key:   "goal",
									Value: "silver",
								},
							},
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "multiple-key-value-pairs",
							Properties: []*resultstore.Property{
								{
									Key:   "silver",
									Value: "medal",
								},
								{
									Key:   "goal",
									Value: "gold",
								},
								{
									Key:   "critical",
									Value: "information",
								},
							},
						},
					},
				},
			},
			expected: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "correct-key",
							Properties: []*resultstore.Property{
								{
									Key:   "goal",
									Value: "gold",
								},
							},
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "multiple-key-value-pairs",
							Properties: []*resultstore.Property{
								{
									Key:   "silver",
									Value: "medal",
								},
								{
									Key:   "goal",
									Value: "gold",
								},
								{
									Key:   "critical",
									Value: "information",
								},
							},
						},
					},
				},
			},
		},
		{
			name: "must match all properties",
			properties: []*configpb.TestGroup_KeyValue{
				{
					Key:   "medal",
					Value: "gold",
				},
				{
					Key:   "ribbon",
					Value: "blue",
				},
			},
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "zero",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "wrong-medal",
							Properties: []*resultstore.Property{
								{
									Key:   "ribbon",
									Value: "blue",
								},
							},
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "wrong-ribbon",
							Properties: []*resultstore.Property{
								{
									Key:   "medal",
									Value: "gold",
								},
							},
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "whole-deal",
							Properties: []*resultstore.Property{
								{
									Key:   "medal",
									Value: "gold",
								},
								{
									Key:   "ribbon",
									Value: "blue",
								},
							},
						},
					},
				},
			},
			expected: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "whole-deal",
							Properties: []*resultstore.Property{
								{
									Key:   "medal",
									Value: "gold",
								},
								{
									Key:   "ribbon",
									Value: "blue",
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actual := filterProperties(tc.results, tc.properties)
			if diff := cmp.Diff(actual, tc.expected, protocmp.Transform()); diff != "" {
				t.Errorf("filterProperties() got unexpected diff (-have, +want):\n%s", diff)
			}
		})
	}
}

func TestFillProperties(t *testing.T) {
	cases := []struct {
		name       string
		properties map[string]string
		result     *resultstore.Test
		match      map[string]bool
		want       map[string]string
	}{
		{
			name: "basically works",
		},
		{
			name:   "still basically works",
			result: &resultstore.Test{},
		},
		{
			name: "simple case no match",
			properties: map[string]string{
				"spongebob": "yellow",
				"patrick":   "pink",
			},
			result: &resultstore.Test{
				TestType: &resultstore.Test_TestCase{
					TestCase: &resultstore.TestCase{
						Properties: []*resultstore.Property{
							{Key: "squidward", Value: "green"},
						},
					},
				},
			},
			want: map[string]string{
				"spongebob": "yellow",
				"patrick":   "pink",
				"squidward": "green",
			},
		},
		{
			name: "simple case with match",
			properties: map[string]string{
				"spongebob": "yellow",
				"mrkrabs":   "red",
			},
			result: &resultstore.Test{
				TestType: &resultstore.Test_TestCase{
					TestCase: &resultstore.TestCase{
						Properties: []*resultstore.Property{
							{Key: "squidward", Value: "blue"},
						},
					},
				},
			},
			match: map[string]bool{
				"squidward": true,
			},
			want: map[string]string{
				"spongebob": "yellow",
				"mrkrabs":   "red",
				"squidward": "blue",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fillProperties(tc.properties, tc.result, tc.match)
			if diff := cmp.Diff(tc.want, tc.properties, protocmp.Transform()); diff != "" {
				t.Errorf("fillProperties() differed (-want, +got): [%s]", diff)
			}
		})
	}
}

func pstr(s string) *string { return &s }

func TestMatchResults(t *testing.T) {
	cases := []struct {
		name     string
		results  []*resultstore.Test
		match    *string
		unmatch  *string
		expected []*resultstore.Test
	}{
		{
			name: "basically works",
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "every",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "thing",
						},
					},
				},
			},
			expected: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "every",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "thing",
						},
					},
				},
			},
		},
		{
			name: "match results",
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "miss",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "yesgopher",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "gopher-yes",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "no",
						},
					},
				},
			},
			match: pstr("gopher"),
			expected: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "yesgopher",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "gopher-yes",
						},
					},
				},
			},
		},
		{
			name: "exclude results with neutral alignments",
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "yesgopher",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "lawful good",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "neutral good",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "chaotic good",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "lawful neutral",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "true neutral",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "chaotic neutral",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "lawful evil",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "neutral evil",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "chaotic evil",
						},
					},
				},
			},
			unmatch: pstr("neutral"),
			expected: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "yesgopher",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "lawful good",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "chaotic good",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "lawful evil",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "chaotic evil",
						},
					},
				},
			},
		},
		{
			name: "exclude the included results",
			results: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "lawful good",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "neutral good",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "chaotic good",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "lawful neutral",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "true neutral",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "chaotic neutral",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "lawful evil",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "neutral evil",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "chaotic evil",
						},
					},
				},
			},
			match:   pstr("good"),
			unmatch: pstr("neutral"),
			expected: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "lawful good",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "chaotic good",
						},
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var match, unmatch *regexp.Regexp
			if tc.match != nil {
				match = regexp.MustCompile(*tc.match)
			}
			if tc.unmatch != nil {
				unmatch = regexp.MustCompile(*tc.unmatch)
			}
			actual := matchResults(tc.results, match, unmatch)
			for i, r := range actual {
				if diff := cmp.Diff(r, tc.expected[i], protocmp.Transform()); diff != "" {
					t.Errorf("matchResults() got unexpected diff (-have, +want):\n%s", diff)
				}
			}

		})
	}
}

func TestGetTestResults(t *testing.T) {
	cases := []struct {
		name      string
		testsuite *resultstore.TestSuite
		want      []*resultstore.Test
	}{
		{
			name: "empty test suite",
			testsuite: &resultstore.TestSuite{
				SuiteName: "Empty Test",
			},
			want: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestSuite{
						TestSuite: &resultstore.TestSuite{
							SuiteName: "Empty Test",
						},
					},
				},
			},
		},
		{
			name:      "nil test suite",
			testsuite: nil,
			want:      nil,
		},
		{
			name: "standard test suite",
			testsuite: &resultstore.TestSuite{
				SuiteName: "Standard test suite",
				Tests: []*resultstore.Test{
					{
						TestType: &resultstore.Test_TestSuite{
							TestSuite: &resultstore.TestSuite{
								SuiteName: "TestDetectJSError",
								Tests: []*resultstore.Test{
									{
										TestType: &resultstore.Test_TestCase{
											TestCase: &resultstore.TestCase{
												CaseName: "TestDetectJSError/Main",
											},
										},
									},
									{
										TestType: &resultstore.Test_TestCase{
											TestCase: &resultstore.TestCase{
												CaseName: "TestDetectJSError/Summary",
											},
										},
									},
									{
										TestType: &resultstore.Test_TestCase{
											TestCase: &resultstore.TestCase{
												CaseName: "TestDetectJSError/Dashboard",
											},
										},
									},
								},
							},
						},
					},
				},
			},
			want: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "TestDetectJSError/Main",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "TestDetectJSError/Summary",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "TestDetectJSError/Dashboard",
						},
					},
				},
			},
		},
		{
			name: "nested test suite",
			testsuite: &resultstore.TestSuite{
				SuiteName: "Nested test suite",
				Tests: []*resultstore.Test{
					{
						TestType: &resultstore.Test_TestSuite{
							TestSuite: &resultstore.TestSuite{
								SuiteName: "TestDetectJSError",
								Tests: []*resultstore.Test{
									{
										TestType: &resultstore.Test_TestCase{
											TestCase: &resultstore.TestCase{
												CaseName: "TestDetectJSError/Main",
											},
										},
									},
									{
										TestType: &resultstore.Test_TestCase{
											TestCase: &resultstore.TestCase{
												CaseName: "TestDetectJSError/Summary",
											},
										},
									},
									{
										TestType: &resultstore.Test_TestSuite{
											TestSuite: &resultstore.TestSuite{
												SuiteName: "TestDetectJSError/Other",
												Tests: []*resultstore.Test{
													{
														TestType: &resultstore.Test_TestCase{
															TestCase: &resultstore.TestCase{
																CaseName: "TestDetectJSError/Misc",
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			want: []*resultstore.Test{
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "TestDetectJSError/Main",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "TestDetectJSError/Summary",
						},
					},
				},
				{
					TestType: &resultstore.Test_TestCase{
						TestCase: &resultstore.TestCase{
							CaseName: "TestDetectJSError/Misc",
						},
					},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := getTestResults(tc.testsuite)
			if diff := cmp.Diff(tc.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("getTestResults(%+v) differed (-want, +got): [%s]", tc.testsuite, diff)
			}
		})
	}
}

func TestMethodRegex(t *testing.T) {
	regexErrYes := regexp.MustCompile("yes")
	regexErrNo := regexp.MustCompile("no")
	type result struct {
		matchMethods      *regexp.Regexp
		unmatchMethods    *regexp.Regexp
		matchMethodsErr   error
		unmatchMethodsErr error
	}
	cases := []struct {
		name string
		tg   *configpb.TestGroup
		want result
	}{
		{
			name: "basically works",
			tg:   &configpb.TestGroup{},
		},
		{
			name: "basic test",
			tg: &configpb.TestGroup{
				TestMethodMatchRegex:   "yes",
				TestMethodUnmatchRegex: "no",
			},
			want: result{
				matchMethods:      regexErrYes,
				unmatchMethods:    regexErrNo,
				matchMethodsErr:   nil,
				unmatchMethodsErr: nil,
			},
		},
		{
			name: "invalid regex test",
			tg: &configpb.TestGroup{
				TestMethodMatchRegex:   "\x8A",
				TestMethodUnmatchRegex: "\x8A",
			},
			want: result{
				matchMethods:      nil,
				unmatchMethods:    nil,
				matchMethodsErr:   nil,
				unmatchMethodsErr: nil,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mm, umm, mmErr, ummErr := testMethodRegex(tc.tg)
			res := result{
				matchMethods:      mm,
				unmatchMethods:    umm,
				matchMethodsErr:   mmErr,
				unmatchMethodsErr: ummErr,
			}
			if diff := cmp.Diff(tc.want.matchMethods, res.matchMethods, protocmp.Transform(), cmp.AllowUnexported(regexp.Regexp{})); diff != "" {
				t.Errorf("testMethodRegex(%+v) differed (-want, +got): [%s]", tc.tg, diff)
			}
			if diff := cmp.Diff(tc.want.unmatchMethods, res.unmatchMethods, protocmp.Transform(), cmp.AllowUnexported(regexp.Regexp{})); diff != "" {
				t.Errorf("testMethodRegex(%+v) differed (-want, +got): [%s]", tc.tg, diff)
			}
		})
	}
}

func TestQueryAfter(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		query string
		when  time.Time
		want  string
	}{
		{
			name: "empty",
			want: "",
		},
		{
			name:  "zero",
			query: queryProw,
			when:  time.Time{},
			want:  "invocation_attributes.labels:\"prow\" timing.start_time>=\"0001-01-01T00:00:00Z\"",
		},
		{
			name:  "basic",
			query: queryProw,
			when:  now,
			want:  fmt.Sprintf("invocation_attributes.labels:\"prow\" timing.start_time>=\"%s\"", now.UTC().Format(time.RFC3339)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := queryAfter(tc.query, tc.when)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("queryAfter(%q, %v) differed (-want, +got): %s", tc.query, tc.when, diff)
			}
		})
	}
}

func TestSearch(t *testing.T) {
	twoDaysAgo := time.Now().AddDate(0, 0, -2)
	testQueryAfter := queryAfter(queryProw, twoDaysAgo)
	cases := []struct {
		name    string
		stop    time.Time
		client  *fakeClient
		want    []string
		wantErr bool
	}{
		{
			name:    "nil",
			wantErr: true,
		},
		{
			name:    "empty",
			client:  &fakeClient{},
			wantErr: true,
		},
		{
			name: "basic",
			client: &fakeClient{
				searches: map[string][]string{
					testQueryAfter: {"id-1", "id-2", "id-3"},
				},
			},
			stop: twoDaysAgo,
			want: []string{"id-1", "id-2", "id-3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dlClient *DownloadClient
			if tc.client != nil {
				dlClient = &DownloadClient{client: tc.client}
			}
			got, err := search(context.Background(), logrus.WithField("case", tc.name), dlClient, "my-project", tc.stop)
			if err != nil && !tc.wantErr {
				t.Errorf("search() errored: %v", err)
			} else if err == nil && tc.wantErr {
				t.Errorf("search() did not error as expected")
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("search() differed (-want, +got): %s", diff)
			}
		})
	}
}

func TestMostRecent(t *testing.T) {
	now := time.Now()
	oneHourAgo := now.Add(-1 * time.Hour)
	sixHoursAgo := now.Add(-6 * time.Hour)
	cases := []struct {
		name  string
		times []time.Time
		want  time.Time
	}{
		{
			name: "empty",
			want: time.Time{},
		},
		{
			name:  "single",
			times: []time.Time{oneHourAgo},
			want:  oneHourAgo,
		},
		{
			name:  "mix",
			times: []time.Time{now, oneHourAgo, sixHoursAgo},
			want:  now,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mostRecent(tc.times)
			if !tc.want.Equal(got) {
				t.Errorf("stopFromColumns() differed; got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStopFromColumns(t *testing.T) {
	now := time.Now()
	oneHourAgo := now.Add(-1 * time.Hour)
	sixHoursAgo := now.Add(-6 * time.Hour)
	b, _ := oneHourAgo.MarshalText()
	oneHourHint := string(b)
	cases := []struct {
		name string
		cols []updater.InflatedColumn
		want time.Time
	}{
		{
			name: "empty",
			want: time.Time{},
		},
		{
			name: "column start",
			cols: []updater.InflatedColumn{
				{
					Column: &statepb.Column{
						Started: float64(oneHourAgo.Unix() * 1000),
					},
				},
			},
			want: oneHourAgo.Truncate(time.Second),
		},
		{
			name: "column hint",
			cols: []updater.InflatedColumn{
				{
					Column: &statepb.Column{
						Started: float64(sixHoursAgo.Unix() * 1000),
						Hint:    oneHourHint,
					},
				},
			},
			want: oneHourAgo.Truncate(time.Second),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stopFromColumns(logrus.WithField("case", tc.name), tc.cols)
			if !tc.want.Equal(got) {
				t.Errorf("stopFromColumns() differed; got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUpdateStop(t *testing.T) {
	now := time.Now()
	oneHourAgo := now.Add(-1 * time.Hour)
	sixHoursAgo := now.Add(-6 * time.Hour)
	twoDaysAgo := now.AddDate(0, 0, -2)
	twoWeeksAgo := now.AddDate(0, 0, -14)
	oneMonthAgo := now.AddDate(0, 0, -30)
	b, _ := oneHourAgo.MarshalText()
	oneHourHint := string(b)
	cases := []struct {
		name        string
		tg          *configpb.TestGroup
		cols        []updater.InflatedColumn
		defaultStop time.Time
		reprocess   time.Duration
		want        time.Time
	}{
		{
			name: "empty",
			want: twoDaysAgo.Truncate(time.Second),
		},
		{
			name:      "reprocess",
			reprocess: 14 * 24 * time.Hour,
			want:      twoWeeksAgo.Truncate(time.Second),
		},
		{
			name: "days of results",
			tg: &configpb.TestGroup{
				DaysOfResults: 7,
			},
			want: twoWeeksAgo.Truncate(time.Second),
		},
		{
			name:        "default stop, no days of results",
			defaultStop: oneMonthAgo,
			want:        twoDaysAgo.Truncate(time.Second),
		},
		{
			name: "default stop earlier than days of results",
			tg: &configpb.TestGroup{
				DaysOfResults: 7,
			},
			defaultStop: oneMonthAgo,
			want:        twoWeeksAgo.Truncate(time.Second),
		},
		{
			name: "default stop later than days of results",
			tg: &configpb.TestGroup{
				DaysOfResults: 30,
			},
			defaultStop: twoWeeksAgo,
			want:        twoWeeksAgo.Truncate(time.Second),
		},
		{
			name: "column start",
			cols: []updater.InflatedColumn{
				{
					Column: &statepb.Column{
						Started: float64(oneHourAgo.Unix() * 1000),
					},
				},
			},
			defaultStop: twoWeeksAgo,
			want:        oneHourAgo.Truncate(time.Second),
		},
		{
			name: "column hint",
			cols: []updater.InflatedColumn{
				{
					Column: &statepb.Column{
						Started: float64(sixHoursAgo.Unix() * 1000),
						Hint:    oneHourHint,
					},
				},
			},
			defaultStop: twoWeeksAgo,
			want:        oneHourAgo.Truncate(time.Second),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := updateStop(logrus.WithField("testcase", tc.name), tc.tg, now, tc.cols, tc.defaultStop, tc.reprocess)
			if !tc.want.Equal(got) {
				t.Errorf("updateStop() differed; got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIdentifyBuild(t *testing.T) {
	cases := []struct {
		name   string
		result *invocation
		tg     *configpb.TestGroup
		want   string
	}{
		{
			name: "no override configurations",
			result: &invocation{
				InvocationProto: &resultstore.Invocation{
					Name: invocationName("id-123"),
					Id: &resultstore.Invocation_Id{
						InvocationId: "id-123",
					},
				},
			},
			want: "",
		},
		{
			name: "override by non-existent property key",
			result: &invocation{
				InvocationProto: &resultstore.Invocation{
					Name: invocationName("id-1234"),
					Id: &resultstore.Invocation_Id{
						InvocationId: "id-1234",
					},
					Properties: []*resultstore.Property{
						{Key: "Luigi", Value: "Peaches"},
						{Key: "Bowser", Value: "Pingui"},
					},
				},
			},
			tg: &configpb.TestGroup{
				BuildOverrideConfigurationValue: "Mario",
			},
			want: "",
		},
		{
			name: "override by existent property key",
			result: &invocation{
				InvocationProto: &resultstore.Invocation{
					Name: invocationName("id-1234"),
					Id: &resultstore.Invocation_Id{
						InvocationId: "id-1234",
					},
					Properties: []*resultstore.Property{
						{Key: "Luigi", Value: "Peaches"},
						{Key: "Bowser", Value: "Pingui"},
						{Key: "Waluigi", Value: "Wapeaches"},
					},
				},
			},
			tg: &configpb.TestGroup{
				BuildOverrideConfigurationValue: "Waluigi",
			},
			want: "Wapeaches",
		},
		{
			name: "override by build time strf",
			result: &invocation{
				InvocationProto: &resultstore.Invocation{
					Name: invocationName("id-1234"),
					Id: &resultstore.Invocation_Id{
						InvocationId: "id-1234",
					},
					Timing: &resultstore.Timing{
						StartTime: &timestamppb.Timestamp{
							Seconds: 1689881216,
							Nanos:   27847,
						},
					},
				},
			},
			tg: &configpb.TestGroup{
				BuildOverrideStrftime: "%Y-%m-%d-%H",
			},
			want: "2023-07-20-19",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := identifyBuild(tc.tg, tc.result)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("queryAfter(...) differed (-want, +got): %s", diff)
			}
		})
	}
}

func TestIncludeStatus(t *testing.T) {
	cases := []struct {
		name string
		tg   *configpb.TestGroup
		sar  *singleActionResult
		want bool
	}{
		{
			name: "unspecifies status - not included",
			sar: &singleActionResult{
				ConfiguredTargetProto: &resultstore.ConfiguredTarget{
					StatusAttributes: &resultstore.StatusAttributes{
						Status: resultstore.Status_STATUS_UNSPECIFIED,
					},
				},
			},
			want: false,
		},
		{
			name: "built status and ignored - not included",
			tg: &configpb.TestGroup{
				IgnoreBuilt: true,
			},
			sar: &singleActionResult{
				ConfiguredTargetProto: &resultstore.ConfiguredTarget{
					StatusAttributes: &resultstore.StatusAttributes{
						Status: resultstore.Status_BUILT,
					},
				},
			},
			want: false,
		},
		{
			name: "built status and not ignored - included",
			tg: &configpb.TestGroup{
				IgnoreSkip: true,
			},
			sar: &singleActionResult{
				ConfiguredTargetProto: &resultstore.ConfiguredTarget{
					StatusAttributes: &resultstore.StatusAttributes{
						Status: resultstore.Status_BUILT,
					},
				},
			},
			want: true,
		},
		{
			name: "running status and ignored - not included",
			tg: &configpb.TestGroup{
				IgnorePending: true,
			},
			sar: &singleActionResult{
				ConfiguredTargetProto: &resultstore.ConfiguredTarget{
					StatusAttributes: &resultstore.StatusAttributes{
						Status: resultstore.Status_TESTING,
					},
				},
			},
			want: false,
		},
		{
			name: "running status and not ignored - included",
			tg: &configpb.TestGroup{
				IgnoreSkip: true,
			},
			sar: &singleActionResult{
				ConfiguredTargetProto: &resultstore.ConfiguredTarget{
					StatusAttributes: &resultstore.StatusAttributes{
						Status: resultstore.Status_TESTING,
					},
				},
			},
			want: true,
		},
		{
			name: "skipped status and ignored - not included",
			tg: &configpb.TestGroup{
				IgnoreSkip: true,
			},
			sar: &singleActionResult{
				ConfiguredTargetProto: &resultstore.ConfiguredTarget{
					StatusAttributes: &resultstore.StatusAttributes{
						Status: resultstore.Status_SKIPPED,
					},
				},
			},
			want: false,
		},
		{
			name: "other status - included",
			tg: &configpb.TestGroup{
				IgnoreSkip: true,
			},
			sar: &singleActionResult{
				ConfiguredTargetProto: &resultstore.ConfiguredTarget{
					StatusAttributes: &resultstore.StatusAttributes{
						Status: resultstore.Status_FAILED,
					},
				},
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := includeStatus(tc.tg, tc.sar)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("includeStatus(...) differed (-want, +got): %s", diff)
			}
		})
	}
}

func TestGroupInvocations(t *testing.T) {
	cases := []struct {
		name        string
		tg          *configpb.TestGroup
		invocations []*invocation
		want        []*invocationGroup
	}{
		{
			name: "grouping by build - build override by configuration value",
			tg: &configpb.TestGroup{
				PrimaryGrouping:                 configpb.TestGroup_PRIMARY_GROUPING_BUILD,
				BuildOverrideConfigurationValue: "my-property",
			},
			invocations: []*invocation{
				{
					InvocationProto: &resultstore.Invocation{
						Name: invocationName("uuid-123"),
						Id: &resultstore.Invocation_Id{
							InvocationId: "uuid-123",
						},
						Properties: []*resultstore.Property{
							{
								Key:   "my-property",
								Value: "my-value-1",
							},
						},
						Timing: &resultstore.Timing{
							StartTime: &timestamppb.Timestamp{
								Seconds: 1234,
							},
						},
					},
				},
				{
					InvocationProto: &resultstore.Invocation{
						Name: invocationName("uuid-321"),
						Id: &resultstore.Invocation_Id{
							InvocationId: "uuid-321",
						},
						Properties: []*resultstore.Property{
							{
								Key:   "my-property",
								Value: "my-value-2",
							},
						},
					},
				},
			},
			want: []*invocationGroup{
				{
					GroupID: "my-value-1",
					Invocations: []*invocation{
						{
							InvocationProto: &resultstore.Invocation{
								Name: invocationName("uuid-123"),
								Id: &resultstore.Invocation_Id{
									InvocationId: "uuid-123",
								},
								Properties: []*resultstore.Property{
									{
										Key:   "my-property",
										Value: "my-value-1",
									},
								},
								Timing: &resultstore.Timing{
									StartTime: &timestamppb.Timestamp{
										Seconds: 1234,
									},
								},
							},
						},
					},
				},
				{
					GroupID: "my-value-2",
					Invocations: []*invocation{
						{
							InvocationProto: &resultstore.Invocation{
								Name: invocationName("uuid-321"),
								Id: &resultstore.Invocation_Id{
									InvocationId: "uuid-321",
								},
								Properties: []*resultstore.Property{
									{
										Key:   "my-property",
										Value: "my-value-2",
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "grouping by invocation id",
			invocations: []*invocation{
				{
					InvocationProto: &resultstore.Invocation{
						Name: invocationName("uuid-123"),
						Id: &resultstore.Invocation_Id{
							InvocationId: "uuid-123",
						},
						Timing: &resultstore.Timing{
							StartTime: &timestamppb.Timestamp{
								Seconds: 1234,
							},
						},
					},
				},
				{
					InvocationProto: &resultstore.Invocation{
						Name: invocationName("uuid-321"),
						Id: &resultstore.Invocation_Id{
							InvocationId: "uuid-321",
						},
					},
				},
			},
			want: []*invocationGroup{
				{
					GroupID: "uuid-123",
					Invocations: []*invocation{
						{
							InvocationProto: &resultstore.Invocation{
								Name: invocationName("uuid-123"),
								Id: &resultstore.Invocation_Id{
									InvocationId: "uuid-123",
								},
								Timing: &resultstore.Timing{
									StartTime: &timestamppb.Timestamp{
										Seconds: 1234,
									},
								},
							},
						},
					},
				},
				{
					GroupID: "uuid-321",
					Invocations: []*invocation{
						{
							InvocationProto: &resultstore.Invocation{
								Name: invocationName("uuid-321"),
								Id: &resultstore.Invocation_Id{
									InvocationId: "uuid-321",
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := groupInvocations(logrus.WithField("case", tc.name), tc.tg, tc.invocations)
			if diff := cmp.Diff(tc.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("groupInvocations(...) differed (-want, +got): %s", diff)
			}
		})
	}
}

func TestExtractHeaders(t *testing.T) {
	cases := []struct {
		name       string
		isInv      bool
		inv        *invocation
		sar        *singleActionResult
		headerConf *configpb.TestGroup_ColumnHeader
		want       []string
	}{
		{
			name:  "empty invocation",
			isInv: true,
			want:  nil,
		},
		{
			name:  "empty target results",
			isInv: false,
			want:  nil,
		},
		{
			name:  "invocation has a config value present",
			isInv: true,
			inv: &invocation{
				InvocationProto: &resultstore.Invocation{
					Properties: []*resultstore.Property{
						{Key: "field", Value: "green"},
						{Key: "os", Value: "linux"},
					},
				},
			},
			headerConf: &configpb.TestGroup_ColumnHeader{
				ConfigurationValue: "os",
			},
			want: []string{"linux"},
		},
		{
			name:  "invocation doesn't have a config value present",
			isInv: true,
			inv: &invocation{
				InvocationProto: &resultstore.Invocation{
					Properties: []*resultstore.Property{
						{Key: "radio", Value: "head"},
					},
				},
			},
			headerConf: &configpb.TestGroup_ColumnHeader{
				ConfigurationValue: "rainbows",
			},
			want: nil,
		},
		{
			name:  "invocation has labels with prefixes",
			isInv: true,
			inv: &invocation{
				InvocationProto: &resultstore.Invocation{
					InvocationAttributes: &resultstore.InvocationAttributes{
						Labels: []string{"os=linux", "env=prod", "test=fast", "test=hermetic"},
					},
				},
			},
			headerConf: &configpb.TestGroup_ColumnHeader{
				Label: "test=",
			},
			want: []string{"fast", "hermetic"},
		},
		{
			name: "target result has existing properties",
			sar: &singleActionResult{
				ActionProto: &resultstore.Action{
					ActionType: &resultstore.Action_TestAction{
						TestAction: &resultstore.TestAction{
							TestSuite: &resultstore.TestSuite{
								Properties: []*resultstore.Property{
									{Key: "test-property", Value: "fast"},
								},
								Tests: []*resultstore.Test{
									{
										TestType: &resultstore.Test_TestCase{
											TestCase: &resultstore.TestCase{
												Properties: []*resultstore.Property{
													{Key: "test-property", Value: "hermetic"},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			headerConf: &configpb.TestGroup_ColumnHeader{
				Property: "test-property",
			},
			want: []string{"fast", "hermetic"},
		},
		{
			name: "target results doesn't have properties",
			sar: &singleActionResult{
				ActionProto: &resultstore.Action{},
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			switch {
			case tc.isInv:
				got = tc.inv.extractHeaders(tc.headerConf)
			default:
				got = tc.sar.extractHeaders(tc.headerConf)
			}
			if diff := cmp.Diff(tc.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("extractHeaders(...) differed (-want, +got): %s", diff)
			}
		})
	}
}

func TestCompileHeaders(t *testing.T) {
	cases := []struct {
		name          string
		columnHeaders []*configpb.TestGroup_ColumnHeader
		headers       [][]string
		want          []string
	}{
		{
			name: "no custom headers configured",
			want: nil,
		},
		{
			name: "single custom header set with no values fetched",
			columnHeaders: []*configpb.TestGroup_ColumnHeader{
				{Label: "rapid="},
			},
			headers: make([][]string, 1),
			want:    []string{""},
		},
		{
			name: "single custom header set with one value fetched",
			columnHeaders: []*configpb.TestGroup_ColumnHeader{
				{ConfigurationValue: "os"},
			},
			headers: [][]string{
				{"linux"},
			},
			want: []string{"linux"},
		},
		{
			name: "multiple custom headers set, don't list all",
			columnHeaders: []*configpb.TestGroup_ColumnHeader{
				{Label: "os="},
				{ConfigurationValue: "test-duration"},
			},
			headers: [][]string{
				{"linux", "ubuntu"},
				{"30m"},
			},
			want: []string{"*", "30m"},
		},
		{
			name: "multiple custom headers, list 'em all",
			columnHeaders: []*configpb.TestGroup_ColumnHeader{
				{Property: "type", ListAllValues: true},
				{Label: "test=", ListAllValues: true},
			},
			headers: [][]string{
				{"grass", "flying"},
				{"fast", "unit", "hermetic"},
			},
			want: []string{
				"flying||grass",
				"fast||hermetic||unit",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := compileHeaders(tc.columnHeaders, tc.headers)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("compileHeaders(...) differed (-want,+got): %s", diff)
			}
		})
	}
}
