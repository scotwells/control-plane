package server

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/gogo/protobuf/protoc-gen-gogo/generator"
	"github.com/google/uuid"
	fieldmask_utils "github.com/mennanov/fieldmask-utils"
	"github.com/stackpath/control-plane/server/serverpb"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func getResourceTableName(resource protoreflect.MessageDescriptor) string {
	return fmt.Sprintf(
		"%s_resource",
		proto.GetExtension(resource.Options(), annotations.E_Resource).(*annotations.ResourceDescriptor).Singular,
	)
}

func getResourceAnnotation(resource protoreflect.ProtoMessage) *annotations.ResourceDescriptor {
	return proto.GetExtension(
		resource.ProtoReflect().Descriptor().Options(),
		annotations.E_Resource,
	).(*annotations.ResourceDescriptor)
}

func scanResource(
	scanner interface {
		Scan(dest ...interface{}) error
	},
) (*anypb.Any, error) {
	var uid, name, parent, createTime, updateTime, data string
	var deleteTime sql.NullString
	if err := scanner.Scan(&uid, &name, &parent, &createTime, &updateTime, &deleteTime, &data); err != nil {
		return nil, err
	}

	// Create a new any type to unmarshal the resource into
	anyResource := &anypb.Any{}
	if err := protojson.Unmarshal([]byte(data), anyResource); err != nil {
		return nil, err
	}

	resource, err := anyResource.UnmarshalNew()
	if err != nil {
		return nil, err
	}

	// Create a reflection and a new instance of the message
	resourceReflector := resource.ProtoReflect()
	resourceFields := resource.ProtoReflect().Descriptor().Fields()

	// Set the fields the server is responsible for settings
	resourceReflector.Set(resourceFields.ByName("uid"), protoreflect.ValueOfString(uid))
	resourceReflector.Set(resourceFields.ByName("name"), protoreflect.ValueOfString(name))
	createTimeParsed, err := time.Parse(time.RFC3339Nano, createTime)
	if err != nil {
		return nil, err
	}
	updateTimeParsed, err := time.Parse(time.RFC3339Nano, updateTime)
	if err != nil {
		return nil, err
	}
	if deleteTime.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, deleteTime.String)
		if err != nil {
			return nil, err
		}
		resourceReflector.Set(resourceFields.ByName("delete_time"), protoreflect.ValueOfMessage(timestamppb.New(parsed).ProtoReflect()))
	}

	resourceReflector.Set(resourceFields.ByName("create_time"), protoreflect.ValueOfMessage(timestamppb.New(createTimeParsed).ProtoReflect()))
	resourceReflector.Set(resourceFields.ByName("update_time"), protoreflect.ValueOfMessage(timestamppb.New(updateTimeParsed).ProtoReflect()))

	return anypb.New(resource)
}

// Updater func provides an interface that can be used when doing an atomic update
// to a resource. A new instance of the resource should be returned for storage.
// Any fields marked as IMMUTABLE will be overwritten with the existing entry's
// value.
//
// TODO: Add feature for IMMUTABLE check
type updaterFunc func(existing protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error)

// This function will retrieve a resource from the database for updating using the
// provided function. This function can gurantee that no other updates can be made
// to the resource while this update is running. An Aborted error will be returned
// during conflicts. The existing resource will be unmarshalled into its base type.
func (r *resourceServer) atomicUpdateResource(ctx context.Context, resourceName, resourceType string, updater updaterFunc) (*anypb.Any, error) {
	// Verify the requested resource type was registered.
	resourceDescriptor, err := r.GetResourceDescriptor(resourceType)
	if err != nil {
		return nil, err
	}

	// Grab the reflection of the resource for reference to later
	resourceFields := resourceDescriptor.Fields()

	// Start a database transaction so we can atomically update the resource.
	tx, err := r.database.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}

	// Grab the existing resource from the database. This is run
	// in the transaction and will hold a lock.
	existingResource, err := r.getResource(ctx, tx, &serverpb.GetResourceRequest{
		Name:         resourceName,
		ResourceType: resourceType,
	})
	if err != nil {
		return nil, err
	}

	// Unpack the resource before provivding it to the updater function.
	unpacked, err := existingResource.UnmarshalNew()
	if err != nil {
		return nil, err
	}

	// Pass the existing resource so the caller can modify if needed.
	updatedResource, err := updater(unpacked)
	if err != nil {
		return nil, err
	}

	// Verify that the checks do not conflict. This is based
	// off the e-tag of the resource. Nil will be returned for
	// resources that do not have the e-tag fields.
	if updatesConflict(unpacked, updatedResource) {
		// Inform the user there was a conflict and they have to try again.
		return nil, status.Error(codes.Aborted, "resource %q has been modified. please apply your changes to the latest version and try again")
	}

	// Set the update timestamp of the resource if the field exists on the message.
	if updatedField := resourceFields.ByName("update_time"); updatedField != nil {
		// Set the unique ID of the resource message before it's stored in the database.
		updatedResource.ProtoReflect().Set(updatedField, protoreflect.ValueOfMessage(timestamppb.Now().ProtoReflect()))
	}

	// Convert the resource into an Any type so we can store
	// it in the database with it's type information
	anyResource, err := anypb.New(clearOutputOnlyFields(updatedResource))
	if err != nil {
		return nil, err
	}

	// Convert the cloned resource to json that can be stored in the database.
	reqJson, err := protojson.Marshal(anyResource)
	if err != nil {
		return nil, err
	}

	// Prepare the database query to insert the resource into the database.
	statement, err := tx.PrepareContext(ctx, fmt.Sprintf(
		"UPDATE %s SET update_time = $1, %s, data = $2 WHERE name = $3",
		getResourceTableName(updatedResource.ProtoReflect().Descriptor()),
		getResourceDeletion(updatedResource),
	))
	if err != nil {
		return nil, err
	}

	// Insert the resource into the database
	updateRes, err := statement.ExecContext(
		ctx,
		updatedResource.ProtoReflect().Get(resourceFields.ByName("update_time")).Message().Interface().(*timestamppb.Timestamp).AsTime().Format(time.RFC3339Nano),
		reqJson,
		updatedResource.ProtoReflect().Get(resourceFields.ByName("name")).String(),
	)
	if err != nil {
		return nil, err
	}

	if _, err := updateRes.RowsAffected(); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return anypb.New(updatedResource)
}

// Provies the correct deletion update query for a provided resouce.
func getResourceDeletion(resource protoreflect.ProtoMessage) string {
	// Get the value of the deletion timestamp
	deleteTime := resource.ProtoReflect().Get(resource.ProtoReflect().Descriptor().Fields().ByName("delete_time"))
	// When a value was provided, dump it into an SQL update clause
	if deleteTime.Message().IsValid() {
		return fmt.Sprintf("delete_time = '%s'", deleteTime.Message().Interface().(*timestamppb.Timestamp).AsTime().UTC().Format(time.RFC3339Nano))
	} else {
		return fmt.Sprint("delete_time = NULL")
	}
}

// Checks the provided resources to determine if there's a conflict in
// updates within the system. This will check the etag of the updated
// resource and the existing resource match. False will be returned on
// any resources that do not have an etag field.
func updatesConflict(existing, updated protoreflect.ProtoMessage) bool {
	// etag field will always be "etag". Assuem etag field is the same
	// on both provided resources
	etagField := existing.ProtoReflect().Descriptor().Fields().ByName("etag")

	// nil indicates the etag field was not defined
	if etagField == nil {
		// No conflicts when resources do not support etags
		return false
	}

	// Return true when the existing etag and the
	// updated etag are not the same. Caller should
	// ensure that the etag was
	return existing.ProtoReflect().Get(etagField).String() != updated.ProtoReflect().Get(etagField).String()
}

func (r *resourceServer) UndeleteResource(ctx context.Context, req *serverpb.UndeleteResourceRequest) (*anypb.Any, error) {
	return r.atomicUpdateResource(ctx, req.Name, req.ResourceType, func(existing protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
		// Clear the delete_time field to undelete the resource
		existing.ProtoReflect().Clear(existing.ProtoReflect().Descriptor().Fields().ByName("delete_time"))
		return existing, nil
	})
}

func (r *resourceServer) PurgeResource(ctx context.Context, req *serverpb.PurgeResourceRequest) (*serverpb.PurgeResourceResponse, error) {
	// Verify the requested resource type was registered.
	resourceDescriptor, err := r.GetResourceDescriptor(req.ResourceType)
	if err != nil {
		return nil, err
	}

	// Start a database transactions to ensure that the resource can be created atomically.
	tx, err := r.database.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}

	// Prepare the database query to insert the resource into the database.
	statement, err := tx.PrepareContext(ctx, fmt.Sprintf(
		"DELETE FROM %s WHERE name = $1",
		getResourceTableName(resourceDescriptor),
	))
	if err != nil {
		return nil, err
	}

	// Delete the resource in the database
	deleteRes, err := statement.ExecContext(
		ctx,
		req.Name,
	)
	if err != nil {
		return nil, err
	}

	if _, err := deleteRes.RowsAffected(); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &serverpb.PurgeResourceResponse{}, nil
}

// Returns a list of resources that exists with the provided parent
func (r *resourceServer) ListResources(ctx context.Context, req *serverpb.ListResourcesRequest) (*serverpb.ListResourcesResponse, error) {
	// Verify the requested resource type was registered.
	resourceDescriptor, err := r.GetResourceDescriptor(req.ResourceType)
	if err != nil {
		return nil, err
	}

	// Set the default page size when not provided.
	if req.PageSize == 0 {
		req.PageSize = 50
	}

	// Pull the resources from the database.
	statement, err := r.database.PrepareContext(
		ctx,
		fmt.Sprintf(
			"SELECT uid, name, parent, create_time, update_time, data FROM %s WHERE parent = $1 LIMIT %d",
			getResourceTableName(resourceDescriptor),
			req.PageSize,
		),
	)
	if err != nil {
		return nil, err
	}
	res, err := statement.QueryContext(ctx, req.Parent)
	if err != nil {
		return nil, err
	}

	var resources []*anypb.Any
	// Verify we actually got a result from the database
	for res.Next() {
		resource, err := scanResource(res)
		if err != nil {
			return nil, err
		}

		resources = append(resources, resource)
	}

	return &serverpb.ListResourcesResponse{
		Resources: resources,
	}, nil
}

type database interface {
	PrepareContext(context.Context, string) (*sql.Stmt, error)
}

func (r *resourceServer) getResource(ctx context.Context, db database, req *serverpb.GetResourceRequest) (*anypb.Any, error) {
	// Verify the requested resource type was registered.
	resourceDescriptor, err := r.GetResourceDescriptor(req.ResourceType)
	if err != nil {
		return nil, err
	}

	statement, err := db.PrepareContext(
		ctx,
		fmt.Sprintf(
			"SELECT uid, name, parent, create_time, update_time, delete_time, data FROM %s WHERE name = $1",
			getResourceTableName(resourceDescriptor),
		),
	)
	if err != nil {
		return nil, err
	}
	res, err := statement.QueryContext(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	// Verify we actually got a result from the database
	if !res.Next() {
		return nil, status.Error(codes.NotFound, "resource not found")
	}
	// Pull the resource from the database
	return scanResource(res)
}

func (r *resourceServer) GetResource(ctx context.Context, req *serverpb.GetResourceRequest) (*anypb.Any, error) {
	return r.getResource(ctx, r.database, req)
}

// Create a new resource in the server
func (r *resourceServer) CreateResource(ctx context.Context, req *serverpb.CreateResourceRequest) (*anypb.Any, error) {
	// Verify that the provided resource was registered with the server.
	if err := r.assertRegisteredAnyResource(req.Resource); err != nil {
		return nil, err
	}

	resource, err := anypb.UnmarshalNew(req.Resource, proto.UnmarshalOptions{})
	if err != nil {
		return nil, err
	}

	// Grab the reflection of the resource for reference to later
	resourceReflector := resource.ProtoReflect()
	resourceFields := resourceReflector.Descriptor().Fields()

	// Set the unique ID of the resource
	resourceReflector.Set(resourceFields.ByName("uid"), protoreflect.ValueOf(uuid.New().String()))
	// Set the creation timestamp of the resource
	resourceReflector.Set(resourceFields.ByName("create_time"), protoreflect.ValueOfMessage(timestamppb.Now().ProtoReflect()))

	// Set the update timestamp of the resource if the field exists on the message.
	if updatedField := resourceFields.ByName("update_time"); updatedField != nil {
		// Set the unique ID of the resource message before it's stored in the database.
		resourceReflector.Set(updatedField, protoreflect.ValueOfMessage(timestamppb.Now().ProtoReflect()))
	}

	// Convert the resource into an Any type so we can store
	// it in the database with it's type information
	anyResource, err := anypb.New(clearOutputOnlyFields(resource))
	if err != nil {
		return nil, err
	}

	// Convert the cloned resource to json that can be stored in the database.
	reqJson, err := protojson.Marshal(anyResource)
	if err != nil {
		return nil, err
	}

	// Start a database transactions to ensure that the resource can be created atomically.
	tx, err := r.database.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}

	// Verify that a resource with the same name doesn't already exist.
	existing, err := r.getResource(ctx, tx, &serverpb.GetResourceRequest{
		Name:         resourceReflector.Get(resourceFields.ByName("name")).String(),
		ResourceType: string(resourceReflector.Descriptor().FullName()),
	})
	if err != nil && status.Code(err) != codes.NotFound {
		return nil, err
	} else if existing != nil {
		return nil, status.Error(codes.AlreadyExists, "Resource already exists")
	}

	// Prepare the database query to insert the resource into the database.
	statement, err := tx.PrepareContext(ctx, fmt.Sprintf(
		"INSERT INTO %s (uid, name, parent, create_time, update_time, data) VALUES ($1, $2, $3, $4, $5, $6)",
		getResourceTableName(resource.ProtoReflect().Descriptor()),
	))
	if err != nil {
		return nil, err
	}

	// Insert the resource into the database
	res, err := statement.ExecContext(
		ctx,
		resourceReflector.Get(resourceFields.ByName("uid")).String(),
		resourceReflector.Get(resourceFields.ByName("name")).String(),
		req.Parent,
		resourceReflector.Get(resourceFields.ByName("create_time")).Message().Interface().(*timestamppb.Timestamp).AsTime().Format(time.RFC3339Nano),
		resourceReflector.Get(resourceFields.ByName("update_time")).Message().Interface().(*timestamppb.Timestamp).AsTime().Format(time.RFC3339Nano),
		reqJson,
	)
	if err != nil {
		return nil, err
	}

	if _, err := res.RowsAffected(); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return anypb.New(resource)
}

// Get the value of the name field from the resource. Name field
// MUST be present on the resource.
func resourceName(resource *anypb.Any) (string, error) {
	r, err := resource.UnmarshalNew()
	if err != nil {
		return "", err
	}

	return r.ProtoReflect().Get(r.ProtoReflect().Descriptor().Fields().ByName("name")).String(), nil
}

func (r *resourceServer) UpdateResource(ctx context.Context, req *serverpb.UpdateResourceRequest) (*anypb.Any, error) {
	name, err := resourceName(req.Resource)
	if err != nil {
		return nil, err
	}

	// Atomically update a resource and return an error on conflict.
	return r.atomicUpdateResource(ctx, name, req.Resource.TypeUrl, func(existing protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
		// Generate a field mask from the update mask that was provided
		mask, err := fieldmask_utils.MaskFromProtoFieldMask(req.UpdateMask, generator.CamelCase)
		if err != nil {
			return nil, err
		}

		updatedResource, err := req.Resource.UnmarshalNew()
		if err != nil {
			return nil, err
		}

		existingResource, err := req.Resource.UnmarshalNew()
		if err != nil {
			return nil, err
		}

		// Merge the requested resource and the existing resource together.
		if err := fieldmask_utils.StructToStruct(mask, updatedResource, existingResource); err != nil {
			return nil, err
		}

		return updatedResource, nil
	})
}

func (r *resourceServer) DeleteResource(ctx context.Context, req *serverpb.DeleteResourceRequest) (*anypb.Any, error) {
	// Atomically set the deletion timestamp of the resource.
	return r.atomicUpdateResource(ctx, req.Name, req.ResourceType, func(existing protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
		fmt.Printf("%+#v\n", existing)
		// Set the deletion timestamp on the resource
		existing.ProtoReflect().Set(
			// Assume that the resource has a delete_time field defined.
			existing.ProtoReflect().Descriptor().Fields().ByName("delete_time"),
			// Set to the current timestamp.
			protoreflect.ValueOfMessage(timestamppb.Now().ProtoReflect()),
		)
		return existing, nil
	})
}

// This function will return a cloned proto message that has any fields
// with an OUTPUT_ONLY behavior cleared.
func clearOutputOnlyFields(resource proto.Message) proto.Message {
	// Clone the resource and clear the values for anything that is marked as output only
	resourceCopy := proto.Clone(resource)
	for i := 0; i < resource.ProtoReflect().Descriptor().Fields().Len(); i++ {
		// Skip any fields that don't have the Field Behavior annotation
		if !proto.HasExtension(resource.ProtoReflect().Descriptor().Fields().Get(i).Options(), annotations.E_FieldBehavior) {
			continue
		}

		behaviors := proto.GetExtension(
			resource.ProtoReflect().Descriptor().Fields().Get(i).Options(),
			annotations.E_FieldBehavior,
		).([]annotations.FieldBehavior)
		for _, behavior := range behaviors {
			if behavior == annotations.FieldBehavior_OUTPUT_ONLY {
				resourceCopy.ProtoReflect().Clear(resource.ProtoReflect().Descriptor().Fields().Get(i))
			}
		}
	}
	return resourceCopy
}

// This function will return the string value that was provided in
// the provided proto message.
//
// This function expects that the request would provide a filter value in a
// field labeled "filter". An empty string indicates that the provided message
// either does not have a field with the label "filter" or a filter was not
// provided.
func getFilterValue(req proto.Message) string {
	// Apply any filtering if a request was provided.
	filter := req.ProtoReflect().Descriptor().Fields().ByName("filter")
	if filter == nil {
		return ""
	}

	// Get the value that was provided as a filter in the request
	return req.ProtoReflect().Get(filter).String()
}
