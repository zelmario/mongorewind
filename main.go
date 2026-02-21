package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/zelmario/mongorewind/internal/app"
	"github.com/zelmario/mongorewind/internal/enabler"
)

func main() {
	uri := flag.String("uri", "mongodb://localhost:27017", "MongoDB connection URI.\n\tFor replica sets use the full connection string so the driver discovers the primary automatically:\n\t  mongodb://host1:27017,host2:27017,host3:27017/?replicaSet=<name>")
	logFile := flag.String("log", "rewind.log", "Path to the on-disk change log")
	flag.Parse()

	ctx := context.Background()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(*uri))
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer client.Disconnect(ctx)

	if err := client.Ping(ctx, nil); err != nil {
		log.Fatalf("ping MongoDB: %v\n\nMake sure:\n  • MongoDB 6.0+ is running as a replica set or sharded cluster\n  • For replica sets, pass the full URI:\n    --uri \"mongodb://host1:27017,host2:27017/?replicaSet=<name>\"", err)
	}

	fmt.Println("[mongorewind] Connected to MongoDB.")
	fmt.Println("[mongorewind] Enabling changeStreamPreAndPostImages on all collections...")

	count, err := enabler.EnableAll(ctx, client)
	if err != nil {
		log.Fatalf("enable pre-images: %v", err)
	}
	fmt.Printf("[mongorewind] Pre-images enabled on %d collection(s).\n", count)

	a := app.New(client, *logFile)
	if err := a.Run(); err != nil {
		log.Fatal(err)
	}
}
