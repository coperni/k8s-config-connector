// Copyright 2024 Google LLC
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

package mockspannerinstance

import (
	"context"
	"reflect"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	pb "github.com/GoogleCloudPlatform/k8s-config-connector/mockgcp/generated/mockgcp/spanner/admin/instance/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var _ pb.InstanceAdminServer = &SpannerInstanceV1{}

type SpannerInstanceV1 struct {
	*MockService
	pb.UnimplementedInstanceAdminServer
}

func (s *SpannerInstanceV1) GetInstance(ctx context.Context, req *pb.GetInstanceRequest) (*pb.Instance, error) {
	name, err := s.parseInstanceName(req.Name)
	if err != nil {
		return nil, err
	}
	fqn := name.String()

	obj := &pb.Instance{}
	if err := s.storage.Get(ctx, fqn, obj); err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "Instance not found: %s", name.String())
		}
		return nil, err
	}
	return obj, nil
}

func (s *SpannerInstanceV1) CreateInstance(ctx context.Context, req *pb.CreateInstanceRequest) (*longrunningpb.Operation, error) {
	instanceName := req.GetParent() + "/instances/" + req.GetInstanceId()
	name, err := s.parseInstanceName(instanceName)
	if err != nil {
		return nil, err
	}
	fqn := name.String()
	now := timestamppb.Now()

	obj := proto.Clone(req.GetInstance()).(*pb.Instance)
	obj.Name = fqn
	s.populateDefaultsForSpannerInstance(obj, obj)
	obj.State = pb.Instance_READY
	obj.CreateTime = now
	obj.UpdateTime = now
	if err := s.storage.Create(ctx, fqn, obj); err != nil {
		return nil, err
	}
	metadata := &pb.CreateInstanceMetadata{
		Instance:                  obj,
		StartTime:                 now,
		EndTime:                   now,
		ExpectedFulfillmentPeriod: pb.FulfillmentPeriod_FULFILLMENT_PERIOD_NORMAL,
	}
	return s.operations.DoneLRO(ctx, name.String(), metadata, obj)
}

func (s *SpannerInstanceV1) populateDefaultsForSpannerInstance(update, obj *pb.Instance) {
	// At most one of either node_count or processing_units should be present.
	// https://cloud.google.com/spanner/docs/compute-capacity
	// 1 nodeCount equals to 1000 processingUnits
	if 1000*update.NodeCount > update.ProcessingUnits {
		obj.ProcessingUnits = 1000 * update.NodeCount
		obj.NodeCount = update.NodeCount
	} else {
		obj.ProcessingUnits = update.ProcessingUnits
		obj.NodeCount = update.ProcessingUnits / 1000
	}
}

func (s *SpannerInstanceV1) UpdateInstance(ctx context.Context, req *pb.UpdateInstanceRequest) (*longrunningpb.Operation, error) {
	name, err := s.parseInstanceName(req.Instance.Name)
	if err != nil {
		return nil, err
	}
	fqn := name.String()
	obj := &pb.Instance{}
	if err := s.storage.Get(ctx, fqn, obj); err != nil {
		return nil, err
	}
	now := timestamppb.Now()
	obj.UpdateTime = now
	source := reflect.ValueOf(req.Instance)
	target := reflect.ValueOf(obj).Elem()
	for _, path := range req.FieldMask.Paths {
		f := target.FieldByName(path)
		if f.IsValid() && f.CanSet() {
			switch f.Kind() {
			case reflect.Int:
				intVal := source.FieldByName(path).Int()
				f.SetInt(intVal)
			case reflect.String:
				stringVal := source.FieldByName(path).String()
				f.SetString(stringVal)
			}

		}
	}

	s.populateDefaultsForSpannerInstance(req.Instance, obj)
	if err := s.storage.Update(ctx, fqn, obj); err != nil {
		return nil, err
	}
	metadata := &pb.UpdateInstanceMetadata{
		ExpectedFulfillmentPeriod: pb.FulfillmentPeriod_FULFILLMENT_PERIOD_NORMAL,
		Instance:                  obj,
		StartTime:                 now,
		EndTime:                   now,
	}
	return s.operations.DoneLRO(ctx, name.String(), metadata, obj)
}

func (s *SpannerInstanceV1) DeleteInstance(ctx context.Context, req *pb.DeleteInstanceRequest) (*emptypb.Empty, error) {
	name, err := s.parseInstanceName(req.Name)
	if err != nil {
		return nil, err
	}

	fqn := name.String()

	existing := &pb.Instance{}
	if err := s.storage.Delete(ctx, fqn, existing); err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}
