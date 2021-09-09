package main

import (
	"database/sql"
	"log"
	"net"

	_ "github.com/lib/pq"
	"github.com/spf13/cobra"
	"github.com/stackpath/control-plane/features"
	"github.com/stackpath/control-plane/server"
)

// Create the root command
var rootCmd = &cobra.Command{Use: "control-plane"}

// Create a command to start a new control plane
// server that has no resources defined.
var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start a new gRPC server",
	RunE:  serverFunc,
}

func main() {
	startCmd.PersistentFlags().String("grpc.listen-address", "The listening address that the gRPC should bind to", ":8080")
	// Add a new command to run an empty control plane server.
	rootCmd.AddCommand(startCmd)

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

func serverFunc(cmd *cobra.Command, args []string) error {
	log.Print("Opening postgres database")
	db, err := sql.Open("postgres", "postgres://root@localhost:26257/stackpath_tests?sslmode=disable")
	if err != nil {
		log.Fatalf("failed to open new database connection: %v", err)
	}

	listenAddr, _ := cmd.Flags().GetString("grpc.listen-address")
	if err != nil {
		return err
	}

	log.Printf("Dialing TCP address for gRPC server on address %q", listenAddr)
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("failed to get TCP listener: %v", err)
	}

	backend := server.New(db)

	if err := backend.CreateResourceDescriptor(&features.Account{}); err != nil {
		log.Fatalf("Failed to register Account resource: %v", err)
	}

	log.Print("Creating a new gRPC server")
	srv, err := server.GRPCAPI(backend)
	if err != nil {
		log.Fatalf("Failed to create a new gRPC server: %v", err)
	}

	log.Print("Starting gRPC server")
	err = srv.Serve(listener)
	if err != nil {
		log.Fatalf("error when running gRPC server: %v", err)
	}

	return nil
}
