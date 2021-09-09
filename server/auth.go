// Package server provides an implementation of the resource manager
// service that can manage a set of configured resources with the system.
//
// This file contains a set of functions that will enable authorization checks
// on the resources that are being passed into the service.
package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/stackpath/control-plane/server/serverpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/anypb"
)

// Creates a new stream interceptor to verify the calling user has access
// to the requested endpoint. This interceptor will only support one-way
// outbound streaming endpoints.
func authStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		// Check the authorization of the calling user

		// Allow the stream connection to pass through
		return handler(srv, ss)
	}
}

func authUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		// Check that the calling user has access to the requested endpoint
		name := strings.Split(info.FullMethod, "/")
		descr, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name[1]))
		if err != nil {
			return nil, fmt.Errorf("unable to resolve method descriptor for endpoint %v: %v", info.FullMethod, err)
		}

		// Grab the descriptor for the RPC method that's being called
		methodDesc := descr.(protoreflect.ServiceDescriptor).Methods().ByName(protoreflect.Name(name[2]))

		var requiredPermissions []string
		// Grab the required permissions for the endpoint
		if proto.HasExtension(methodDesc.Options(), serverpb.E_RequiredPermissions) {
			requiredPermissions = proto.GetExtension(methodDesc.Options(), serverpb.E_RequiredPermissions).([]string)
		}

		msg := req.(proto.Message).ProtoReflect()

		var resourceName string
		// Messages must meet one of the following criteria to be supported by this authorization interceptor:
		//   * Message MUST define a "parent" field and MAY provide a value
		//   * Message MUST define a "name" field and MUST provide a value
		//   * Message MUST define a "resource" field and MUST provide a value
		if msg.Descriptor().Fields().ByName("name") != nil {
			resourceName = msg.Get(msg.Descriptor().Fields().ByName("name")).String()
		} else if msg.Descriptor().Fields().ByJSONName("parent") != nil {
			resourceName = msg.Get(msg.Descriptor().Fields().ByName("parent")).String()
		} else if msg.Descriptor().Fields().ByJSONName("resource") != nil {
			resource, err := msg.Get(msg.Descriptor().Fields().ByName("resource")).Message().Interface().(*anypb.Any).UnmarshalNew()
			if err != nil {
				return nil, err
			}

			resourceName = resource.ProtoReflect().Get(resource.ProtoReflect().Descriptor().Fields().ByJSONName("name")).String()
		}

		// TODO: Remove. Fake using the variable.
		_ = resourceName

		if len(requiredPermissions) > 0 {
			// TODO: Add authorization checks
			fmt.Printf("Checking that user has %q permission on resource %q\n", requiredPermissions[0], resourceName)
		}

		return handler(ctx, req)
	}
}
