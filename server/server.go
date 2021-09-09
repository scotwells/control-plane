package server

import (
	"database/sql"

	"github.com/stackpath/control-plane/server/serverpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var _ serverpb.ResourcesServer = &resourceServer{}

type API interface {
	// Implements the CRUD operations that should be made available
	// for any registered resource descriptors.
	serverpb.ResourcesServer

	// Registers a new resource descriptor on the server
	CreateResourceDescriptor(message proto.Message) error

	// Retreieves a resource descriptor
	GetResourceDescriptor(resourceType string) (protoreflect.MessageDescriptor, error)

	ListResourceDescriptors() []protoreflect.MessageDescriptor
}

// Creates a new API with no registered resources
func New(db *sql.DB) API {
	return &resourceServer{
		database:  db,
		resources: make(map[string]protoreflect.MessageDescriptor),
	}
}

func GRPCAPI(backend API) (*grpc.Server, error) {
	grpcServer := grpc.NewServer(
		// Add the interceptors that are necessary for the server
		grpc.ChainUnaryInterceptor(authUnaryInterceptor()),
	)

	serverpb.RegisterResourcesServer(grpcServer, backend)

	return grpcServer, nil
}

type resourceServer struct {
	// A map of resources that have been registered with the
	// server. The key of the map will be the `google.api.resource.type`
	// of the annotation that was specified on the resource.
	resources map[string]protoreflect.MessageDescriptor
	database  *sql.DB
}
