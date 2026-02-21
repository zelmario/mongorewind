package enabler

import (
	"context"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

var systemDatabases = map[string]bool{
	"admin":  true,
	"local":  true,
	"config": true,
}

// IsSystem reports whether a db/collection pair is a MongoDB internal namespace.
func IsSystem(db, coll string) bool {
	return systemDatabases[db] || strings.HasPrefix(coll, "system.")
}

// EnableAll enables changeStreamPreAndPostImages on every non-system collection
// across all non-system databases. Returns the number of collections configured.
func EnableAll(ctx context.Context, client *mongo.Client) (int, error) {
	dbNames, err := client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		return 0, err
	}

	count := 0
	for _, dbName := range dbNames {
		if systemDatabases[dbName] {
			continue
		}

		db := client.Database(dbName)
		collNames, err := db.ListCollectionNames(ctx, bson.D{})
		if err != nil {
			continue
		}

		for _, collName := range collNames {
			if IsSystem(dbName, collName) {
				continue
			}
			if err := EnableCollection(ctx, db, collName); err == nil {
				count++
			}
		}
	}

	return count, nil
}

// EnableCollection runs collMod to enable changeStreamPreAndPostImages on a
// single collection. Safe to call if it is already enabled.
func EnableCollection(ctx context.Context, db *mongo.Database, collName string) error {
	return db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: collName},
		{Key: "changeStreamPreAndPostImages", Value: bson.D{
			{Key: "enabled", Value: true},
		}},
	}).Err()
}
