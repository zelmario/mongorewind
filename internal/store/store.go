package store

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Namespace holds the database and collection name from a change event.
type Namespace struct {
	DB   string `bson:"db"`
	Coll string `bson:"coll"`
}

// ChangeEntry is what we persist to disk for each captured change event.
// It carries enough information to invert any operation.
type ChangeEntry struct {
	OperationType            string              `bson:"operationType"`
	ClusterTime              primitive.Timestamp `bson:"clusterTime"`
	NS                       Namespace           `bson:"ns"`
	DocumentKey              bson.Raw            `bson:"documentKey"`
	FullDocument             bson.Raw            `bson:"fullDocument,omitempty"`
	FullDocumentBeforeChange bson.Raw            `bson:"fullDocumentBeforeChange,omitempty"`
}

// KeyString returns the document key as a JSON string, for display purposes.
func (e ChangeEntry) KeyString() string {
	b, err := bson.MarshalExtJSON(e.DocumentKey, false, false)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Writer appends ChangeEntry records to an io.Writer as raw BSON documents.
// Each call to Write is flushed immediately so the log is always durable.
type Writer struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, 64*1024)}
}

func (w *Writer) Write(e ChangeEntry) error {
	data, err := bson.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return w.w.Flush()
}

// ReadAll reads all ChangeEntry records from r. BSON documents are
// self-delimiting (first 4 bytes = little-endian document length), so entries
// are read sequentially until EOF. Truncated trailing data is silently ignored.
func ReadAll(r io.Reader) ([]ChangeEntry, error) {
	var entries []ChangeEntry

	for {
		// Each BSON document starts with a 4-byte little-endian int32 length
		// that includes the length field itself.
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			// EOF at a document boundary = clean end of file.
			// ErrUnexpectedEOF = truncated length = treat as end.
			break
		}

		length := int32(binary.LittleEndian.Uint32(lenBuf[:]))
		if length < 5 || length > 16*1024*1024 {
			return entries, fmt.Errorf("invalid BSON document length %d — log may be corrupt", length)
		}

		data := make([]byte, length)
		copy(data[:4], lenBuf[:])
		if _, err := io.ReadFull(r, data[4:]); err != nil {
			// Truncated document body — stop reading rather than returning an error
			// so a partially-written tail entry does not abort a rewind.
			break
		}

		var entry ChangeEntry
		if err := bson.Unmarshal(data, &entry); err != nil {
			return entries, fmt.Errorf("unmarshal entry: %w", err)
		}
		entries = append(entries, entry)
	}

	return entries, nil
}
