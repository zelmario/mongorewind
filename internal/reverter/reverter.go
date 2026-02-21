package reverter

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"mongorewind/internal/store"
)

type Reverter struct {
	client *mongo.Client
}

func New(client *mongo.Client) *Reverter {
	return &Reverter{client: client}
}

// Rewind applies the inverse of each entry in reverse order (newest → oldest).
func (r *Reverter) Rewind(ctx context.Context, entries []store.ChangeEntry) error {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if err := r.invert(ctx, e); err != nil {
			return fmt.Errorf("revert op %d (%s on %s.%s): %w",
				len(entries)-i, e.OperationType, e.NS.DB, e.NS.Coll, err)
		}
	}
	return nil
}

// invert applies the single inverse operation for a recorded change event:
//
//	insert  → deleteOne
//	update  → replaceOne with pre-image  (upsert in case the doc was later deleted)
//	replace → replaceOne with pre-image
//	delete  → replaceOne/upsert with pre-image  (re-inserts the document)
func (r *Reverter) invert(ctx context.Context, e store.ChangeEntry) error {
	coll := r.client.Database(e.NS.DB).Collection(e.NS.Coll)

	// documentKey is always {"_id": <value>} — safe to use directly as a filter.
	filter := e.DocumentKey

	switch e.OperationType {
	case "insert":
		_, err := coll.DeleteOne(ctx, filter)
		return err

	case "update", "replace":
		if len(e.FullDocumentBeforeChange) == 0 {
			return fmt.Errorf("no pre-image available")
		}
		_, err := coll.ReplaceOne(ctx, filter, e.FullDocumentBeforeChange,
			options.Replace().SetUpsert(true))
		return err

	case "delete":
		if len(e.FullDocumentBeforeChange) == 0 {
			return fmt.Errorf("no pre-image available")
		}
		// Use ReplaceOne+upsert so this is idempotent: if the document was
		// re-inserted between the delete and the rewind, it gets overwritten
		// with the correct pre-image rather than causing a duplicate-key error.
		_, err := coll.ReplaceOne(ctx, filter, e.FullDocumentBeforeChange,
			options.Replace().SetUpsert(true))
		return err

	default:
		return fmt.Errorf("unknown operation type %q", e.OperationType)
	}
}
