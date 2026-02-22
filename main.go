package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"strings"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/zelmario/mongorewind/internal/app"
	"github.com/zelmario/mongorewind/internal/enabler"
)

func main() {
	uri := flag.String("uri", "mongodb://localhost:27017", "MongoDB connection URI.\n\tFor replica sets use the full connection string so the driver discovers the primary automatically:\n\t  mongodb://host1:27017,host2:27017,host3:27017/?replicaSet=<name>")
	logFile := flag.String("log", "rewind.log", "Path to the on-disk change log")
	rewind := flag.Bool("rewind", false, "Send a rewind command to a running mongorewind instance and exit")
	flag.Parse()

	if *rewind {
		sp := socketPath(*logFile)
		if err := sendRewind(sp); err != nil {
			log.Fatalf("rewind: %v", err)
		}
		fmt.Println("[mongorewind] Rewind complete.")
		return
	}

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

// socketPath derives the Unix socket path from the log file path.
// e.g. "rewind.log" → "rewind.sock"
func socketPath(logPath string) string {
	ext := filepath.Ext(logPath)
	return strings.TrimSuffix(logPath, ext) + ".sock"
}

// sendRewind connects to a running mongorewind instance via its Unix socket
// and requests a rewind, blocking until it completes.
func sendRewind(sp string) error {
	conn, err := net.Dial("unix", sp)
	if err != nil {
		return fmt.Errorf("connect to socket %q: %w\n(is mongorewind running with the same --log path?)", sp, err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintln(conn, "rewind"); err != nil {
		return err
	}

	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}

	resp = strings.TrimSpace(resp)
	if strings.HasPrefix(resp, "err:") {
		return fmt.Errorf("%s", strings.TrimPrefix(resp, "err: "))
	}
	return nil
}
