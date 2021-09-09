package features

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-txdb"
	"github.com/cucumber/godog"
	"github.com/cucumber/godog/colors"
	"github.com/cucumber/messages-go/v10"
	_ "github.com/lib/pq"
	"github.com/stackpath/control-plane/server"
	"github.com/stackpath/control-plane/server/serverpb"
	"github.com/stretchr/objx"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

var opt = godog.Options{
	Output:   colors.Colored(os.Stdout),
	Format:   "pretty",
	Paths:    []string{"./"},
	NoColors: false,
	Tags:     "",
	Strict:   true,
}

func init() {
	godog.BindFlags("godog.", flag.CommandLine, &opt)
}

type serverFeature struct {
	responseError error
	server        *grpc.Server
	listener      net.Listener
	clientConn    grpc.ClientConnInterface
	request       interface{}
	response      interface{}
	nextPageToken string
	ctx           context.Context
	db            *sql.DB
	backend       server.API
}

func TestMain(m *testing.M) {
	flag.Parse()

	txdb.Register(
		"txdb",
		"postgres",
		"postgres://root@localhost:26257/resources?sslmode=disable",
		txdb.SavePointOption(nil),
	)

	status := godog.RunWithOptions("godogs", func(s *godog.Suite) {
		FeatureContext(s)
	}, opt)

	if st := m.Run(); st > status {
		status = st
	}
	os.Exit(status)
}

func (f *serverFeature) iWillReceiveAnErrorWithCode(stringCode string) error {
	expectedCode := new(codes.Code)
	if err := expectedCode.UnmarshalJSON([]byte(stringCode)); err != nil {
		return fmt.Errorf("invalid gRPC code: %v", err)
	}

	if s, ok := status.FromError(f.responseError); !ok {
		return fmt.Errorf("error was not able to be converted to a gRPC status: %v", f.responseError)
	} else if s.Code() != *expectedCode {
		return fmt.Errorf("received error code %q, expected code %q", s.Code().String(), expectedCode.String())
	}
	return nil
}

// Verifies that the response error contains specific error details. This will perform strict
// validation so that if any unknown error details are included in the error, the assertion will
// fail.
func (f *serverFeature) theErrorDetailsWillBeForTheFollowingFields(details *godog.Table) error {
	// Verify the response is a gRPC error
	errStatus, ok := status.FromError(f.responseError)
	if !ok {
		return fmt.Errorf("error was not able to be converted to a gRPC status: %v", f.responseError)
	}

	// Verify the error contains a BadRequest error details
	var badRequest *errdetails.BadRequest
	for _, detail := range errStatus.Details() {
		if v, ok := detail.(*errdetails.BadRequest); ok {
			badRequest = v
			break
		}
	}
	if badRequest == nil {
		return fmt.Errorf("response error did not contain a bad request error detail")
	}

	// Simple check to see if the number of field errors we're expecting matches the actual
	if len(details.Rows) != len(badRequest.FieldViolations) {
		return fmt.Errorf("expected %d field violations, got %d: %+v", len(details.Rows), len(badRequest.FieldViolations), badRequest.FieldViolations)
	}

	// Check each expected entry and compare to the actual field errors
	for _, row := range details.Rows {
		// Try to find a matching field error
		var fieldViolation *errdetails.BadRequest_FieldViolation
		for i, violation := range badRequest.FieldViolations {
			if violation.Field == row.Cells[0].Value {
				fieldViolation = badRequest.FieldViolations[i]
				break
			}
		}

		if fieldViolation == nil {
			return fmt.Errorf("unable to find field violation for field %q", row.Cells[0].Value)
		}

		if fieldViolation.Description != row.Cells[1].Value {
			return fmt.Errorf("expected error description to be %q, got %q", row.Cells[1].Value, fieldViolation.Description)
		}
	}
	return nil
}

func (f *serverFeature) iWillReceiveASuccessfulResponse() error {
	if f.responseError != nil {
		return fmt.Errorf(
			"expected a successful response, but received an error: %v: %v",
			f.responseError,
			status.Convert(f.responseError).Details(),
		)
	}
	return nil
}

func (f *serverFeature) theResponseValueWillBe(path, expected string) error {
	// Turn the response to json to more easily get a map[string]interface{}
	r, _ := protojson.Marshal(f.response.(protoreflect.ProtoMessage))
	actual := objx.MustFromJSON(string(r)).Get(path).String()
	if actual != expected {
		return fmt.Errorf("expected '%s' to be '%s', got '%s'", path, expected, actual)
	}
	return nil
}

func (f *serverFeature) theResponseValueWillHaveLength(path string, expectedLen int) error {
	r, _ := protojson.Marshal(f.response.(protoreflect.ProtoMessage))
	actual := objx.MustFromJSON(string(r)).Get(path).Data()

	actualLen := 0
	if a, ok := actual.([]interface{}); ok {
		actualLen = len(a)
	}
	if actualLen != expectedLen {
		return fmt.Errorf("expected length of %s to be %d but it was %d", path, expectedLen, actualLen)
	}

	return nil
}

func (f *serverFeature) stashingTheNextPageTokenFromTheResponse() error {
	if t, ok := f.response.(interface{ GetNextPageToken() string }); !ok {
		return fmt.Errorf("the response does satisfy the next page token interface")
	} else if t.GetNextPageToken() == "" {
		return fmt.Errorf("the response does not contain a next page token")
	} else {
		f.nextPageToken = t.GetNextPageToken()
		return nil
	}
}

func (f *serverFeature) usingTheStashedNextPageToken() error {
	// verify the request satisfies the page token interface
	if _, ok := f.request.(interface{ GetPageToken() string }); !ok {
		return fmt.Errorf("the response does satisfy the next page token interface")
	}

	r, _ := protojson.Marshal(f.request.(protoreflect.ProtoMessage))
	updated := objx.MustFromJSON(string(r)).Set("pageToken", f.nextPageToken).MustJSON()
	request := reflect.ValueOf(f.request).Interface()
	if err := protojson.Unmarshal([]byte(updated), request.(protoreflect.ProtoMessage)); err != nil {
		return fmt.Errorf("failed to update the page token in the request: %v", err)
	}

	f.request = request
	return nil
}

func (f *serverFeature) theResourceIsRegistered(resourceType string) error {
	var resource protoreflect.Message
	// Collect the names of all the registered types in the system
	var registeredTypes []string

	protoregistry.GlobalTypes.RangeMessages(func(message protoreflect.MessageType) bool {
		// The resource should be mapped by the name
		if message.Descriptor().FullName() == protoreflect.FullName(resourceType) {
			resource = message.New()
		}

		registeredTypes = append(registeredTypes, string(message.Descriptor().FullName()))

		// Go through all the registered types
		return true
	})

	// Return an error if the resource couldn't be found
	if resource == nil {
		// Dump the known error types to help with test development
		return fmt.Errorf(
			"could not find registered type: %v\nRegistered Types:\n  - %v",
			resourceType,
			strings.Join(registeredTypes, "\n  - "),
		)
	}

	// Create the resource on the backend
	return f.backend.CreateResourceDescriptor(resource.Interface())
}

func (f *serverFeature) callGRPCMethodFromInput(message protoreflect.ProtoMessage) func(*messages.PickleStepArgument_PickleDocString) error {
	return func(resourcesJSON *messages.PickleStepArgument_PickleDocString) error {
		// Get the Request message based on the type specified in the message
		if err := protojson.Unmarshal([]byte(resourcesJSON.Content), message); err != nil {
			return fmt.Errorf("failed to unmarshal resource %q: %v", message.ProtoReflect().Descriptor().FullName(), err)
		}

		// Find the RPC method that accepts the message as an input parameter.
		var method protoreflect.MethodDescriptor
		protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
			// Go through all of the registered services
			for i := 0; i < file.Services().Len(); i++ {
				srv := file.Services().Get(i)
				for j := 0; j < srv.Methods().Len(); j++ {
					serviceMethod := srv.Methods().Get(j)
					// Check if the current method input matches the same name as the request object
					if serviceMethod.Input().FullName() == message.ProtoReflect().Descriptor().FullName() {
						method = serviceMethod
						return false
					}
				}
			}
			return true
		})
		if method == nil {
			return fmt.Errorf("could not find a method in the protoregistry that accepts the message type %v", message.ProtoReflect().Descriptor().FullName())
		}

		// Build the fully qualified name of the method that should be invoked
		methodName := string(method.Parent().FullName()) + "/" + string(method.Name())
		// Grab the response message descriptor so we can use the type information
		messageType, err := protoregistry.GlobalTypes.FindMessageByName(method.Output().FullName())
		if err != nil {
			return err
		}

		// Grab a new instance of the proto response message. This should be guaranteed to be
		// registered in the Proto registry since it came from the method descriptor
		f.response = messageType.New().Interface()

		// Invoke the API call
		f.responseError = f.clientConn.Invoke(f.ctx, methodName, message, f.response)

		return nil
	}
}

func (f *serverFeature) noResourcesAreRegistered() error {
	// Do nothing
	return nil
}

func (f *serverFeature) theResponseValueWillBeWithinFromNow(path, threshold string) error {
	duration, err := time.ParseDuration(threshold)
	if err != nil {
		return fmt.Errorf("invalid duration %q provided: %v", threshold, err)
	}

	// Must emit defaults so that we can find fields that are set to their default value
	marshaller := protojson.MarshalOptions{EmitUnpopulated: true}
	// Turn the response to json to more easily get a map[string]interface{}
	r, err := marshaller.Marshal(f.response.(protoreflect.ProtoMessage))
	if err != nil {
		return fmt.Errorf("failed to marshal response from RPC endpoint: %v", err)
	}

	actual := objx.MustFromJSON(string(r)).Get(path).String()
	if actual == "" {
		return fmt.Errorf("empty timestamp found in field %q", path)
	}
	now := time.Now().UTC()

	// The field must parse as a valid RFC3999Nano timestamp or it wasn't a date field
	actualTime, err := time.Parse(time.RFC3339Nano, actual)
	if err != nil {
		return fmt.Errorf("failed to parse field %q as a RFC3339Nano timestamp: %v\nResponse: %v", path, err, string(r))
	}
	// Verify that the field was within the threshold from the curren timestamp
	if actualTime.After(now.Add(duration)) || actualTime.Before(now.Add(duration*-1)) {
		return fmt.Errorf("expected %q to be within %q from %q", actual, threshold, now.Format(time.RFC3339Nano))
	}

	return nil
}

func (f *serverFeature) registerSteps(suite *godog.Suite) {
	suite.Step(`^the resource "([^"]*)" is registered$`, f.theResourceIsRegistered)
	suite.Step(`^creating the following resource:$`, f.callGRPCMethodFromInput(&serverpb.CreateResourceRequest{}))
	suite.Step(`^getting the following resource:$`, f.callGRPCMethodFromInput(&serverpb.GetResourceRequest{}))
	suite.Step(`^deleting the following resource:$`, f.callGRPCMethodFromInput(&serverpb.DeleteResourceRequest{}))
	suite.Step(`^updating the following resource:$`, f.callGRPCMethodFromInput(&serverpb.UpdateResourceRequest{}))
	suite.Step(`^undeleting the following resource$`, f.callGRPCMethodFromInput(&serverpb.UndeleteResourceRequest{}))
	suite.Step(`^purging the following resource$`, f.callGRPCMethodFromInput(&serverpb.PurgeResourceRequest{}))
	suite.Step(`^I will receive an error with code ("[^"]*")$`, f.iWillReceiveAnErrorWithCode)
	suite.Step(`^the BadRequest error details will be for the following fields$`, f.theErrorDetailsWillBeForTheFollowingFields)
	suite.Step(`^I will receive a successful response$`, f.iWillReceiveASuccessfulResponse)
	suite.Step(`^the response value "([^"]*)" will be "([^"]*)"$`, f.theResponseValueWillBe)
	suite.Step(`^the response value "([^"]*)" will have a length of (\d+)$`, f.theResponseValueWillHaveLength)
	suite.Step(`^the response value "([^"]*)" will be within "([^"]*)" from now$`, f.theResponseValueWillBeWithinFromNow)
	suite.Step(`^stashing the next page token from the response$`, f.stashingTheNextPageTokenFromTheResponse)
	suite.Step(`^using the stashed next page token$`, f.usingTheStashedNextPageToken)
	suite.Step(`^no resources are registered$`, f.noResourcesAreRegistered)
}

func FeatureContext(s *godog.Suite) {
	var err error

	feature := &serverFeature{}
	feature.registerSteps(s)

	s.BeforeScenario(func(*messages.Pickle) {
		feature.listener, err = net.Listen("tcp", ":33000")
		if err != nil {
			log.Fatalf("failed to create tcp listener: %v", err)
		}

		feature.clientConn, err = grpc.Dial(feature.listener.Addr().String(), grpc.WithInsecure())
		if err != nil {
			log.Fatalf("failed to create client connection: %v", err)
		}
		feature.ctx = context.Background()
		feature.db, err = sql.Open("txdb", "postgres://root@localhost:26257/resources?sslmode=disable")
		if err != nil {
			log.Fatalf("failed to open new database connection: %v", err)
		}

		if _, err := feature.db.Exec("CREATE DATABASE IF NOT EXISTS resources"); err != nil {
			log.Fatalf("failed to create database: %v", err)
		}

		// Create a new API that should be used for the features. This will
		// be empty by default. Test cases must register the types they want
		// to exist in the server.
		feature.backend = server.New(feature.db)

		api, err := server.GRPCAPI(feature.backend)
		if err != nil {
			log.Fatalf("failed to create new API server: %v", err)
		}
		feature.server = api

		// Start the server in the background
		go feature.server.Serve(feature.listener)
	})

	s.AfterScenario(func(*messages.Pickle, error) {
		feature.listener.Close()
		feature.server.Stop()
		if _, err := feature.db.Exec("DROP DATABASE IF EXISTS resources"); err != nil {
			log.Fatalf("failed to delete database: %v", err)
		}
		feature.db.Close()
	})
}
