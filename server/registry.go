package server

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
)

func (r *resourceServer) CreateResourceDescriptor(message proto.Message) error {
	// Get the Message Descriptor for the message so we can
	// inspect the proto options that are defined.
	resource := message.ProtoReflect().Descriptor()
	// Verify the message provides the necessary resource extension.
	if !proto.HasExtension(resource.Options(), annotations.E_Resource) {
		return fmt.Errorf(
			"google.api.resource annotation is required for storage registration: %s does not have google.api.resource annoitation",
			resource.Name(),
		)
	}

	// Create the table in the database for the resource
	_, err := r.database.ExecContext(context.TODO(), fmt.Sprintf(`
	CREATE TABLE IF NOT EXISTS %s (
		uid                  UUID NOT NULL,
		name                 STRING NOT NULL,
		parent               STRING NOT NULL,
		data                 TEXT NOT NULL,
		create_time          TIMESTAMP,
		update_time          TIMESTAMP,
		delete_time          TIMESTAMP,
		CONSTRAINT "primary" PRIMARY KEY (uid ASC),
		CONSTRAINT resource_name_unique UNIQUE (name),
        FAMILY "primary" (uid, name, parent, create_time, update_time),
		FAMILY "data" (data)
	)`, getResourceTableName(resource)))
	if err != nil {
		return err
	}

	// Add the resource message descriptor to our mapping of types that exist.
	// TODO: Add support for multiple versions
	r.resources[string(resource.FullName())] = resource

	return nil
}

// Gets the resource message descriptor for the provided type. This will return an
// Unimplemented error when no resource descriptor has been registered.
func (r *resourceServer) GetResourceDescriptor(resourceType string) (protoreflect.MessageDescriptor, error) {
	descriptors := r.resources[resourceType]
	if descriptors == nil {
		return nil, r.unknownResourceError(resourceType)
	}
	return descriptors, nil
}

func (r *resourceServer) unknownResourceError(resourceType string) error {
	errStatus := status.Newf(codes.Unimplemented, "Unknwon resource type provided: %v", resourceType)

	var types []string
	for _, resource := range r.ListResourceDescriptors() {
		types = append(types, string(resource.FullName()))
	}

	errStatus, _ = errStatus.WithDetails(&errdetails.BadRequest{
		FieldViolations: []*errdetails.BadRequest_FieldViolation{
			{
				Field:       "@type",
				Description: fmt.Sprintf("Known resource types: %s", strings.Join(types, ", ")),
			},
		},
	})

	return errStatus.Err()
}

func (r *resourceServer) ListResourceDescriptors() []protoreflect.MessageDescriptor {
	descriptors := make([]protoreflect.MessageDescriptor, 0, len(r.resources))
	for i := range r.resources {
		descriptors = append(descriptors, r.resources[i])
	}
	return descriptors
}

func (r *resourceServer) assertRegisteredResourceType(resourceType string) error {
	if r.resources[resourceType] == nil {
		return nil
	}
	return r.unknownResourceError(resourceType)
}

func (r *resourceServer) assertRegisteredAnyResource(resource *anypb.Any) error {
	if resourceDescriptor := r.resources[resource.TypeUrl]; resourceDescriptor == nil {
		return r.unknownResourceError(resource.TypeUrl)
	}
	return nil
}
