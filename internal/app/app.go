package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/term"

	"github.com/zelmario/mongorewind/internal/enabler"
	"github.com/zelmario/mongorewind/internal/reverter"
	"github.com/zelmario/mongorewind/internal/store"
)

type collStats struct {
	inserts, updates, deletes int
}

type displayState struct {
	watching  bool
	count     int
	stats     map[string]collStats // key: "db.coll"
	messages  []string
	termCols  int
	termRows  int
}

type App struct {
	client  *mongo.Client
	logPath string

	// mu protects all mutable fields below.
	mu       sync.Mutex
	entries  []store.ChangeEntry
	stats    map[string]*collStats // key: "db.coll"
	messages []string
	watching bool

	logFile *os.File
	writer  *store.Writer

	watchCancel context.CancelFunc
	watchDone   chan struct{}

	// redrawCh is signalled whenever state changes. A rate-limited goroutine
	// drains it and calls redraw() at most once per 100 ms.
	redrawCh chan struct{}

	// outMu serialises all terminal writes.
	outMu sync.Mutex
}

func New(client *mongo.Client, logPath string) *App {
	return &App{
		client:   client,
		logPath:  logPath,
		stats:    make(map[string]*collStats),
		redrawCh: make(chan struct{}, 1),
	}
}

// ── public entry point ────────────────────────────────────────────────────────

func (a *App) Run() error {
	os.Remove(a.logPath)
	f, err := os.OpenFile(a.logPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", a.logPath, err)
	}
	defer f.Close()
	a.logFile = f
	a.writer = store.NewWriter(f)

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("set raw terminal mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		term.Restore(int(os.Stdin.Fd()), oldState)
		fmt.Print("\033[?25h\r\n") // restore cursor, move to next line
		os.Exit(0)
	}()

	if err := a.startWatching(); err != nil {
		a.addMessage("Could not start watching: %v", err)
	}
	a.redraw()

	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			break
		}
		switch buf[0] {
		case 'r', 'R':
			a.doRewind()
		case 'c', 'C':
			a.clearLog()
			a.redraw()
		case 'q', 'Q', 3:
			a.stopWatching()
			a.outMu.Lock()
			fmt.Print("\033[?25h\r\n") // restore cursor, move to next line
			a.outMu.Unlock()
			return nil
		}
	}
	return nil
}

// ── watch lifecycle ───────────────────────────────────────────────────────────

func (a *App) startWatching() error {
	a.mu.Lock()
	if a.watching {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())

	csOpts := options.ChangeStream().
		SetFullDocument(options.UpdateLookup).
		SetFullDocumentBeforeChange(options.WhenAvailable)

	cs, err := a.client.Watch(ctx, mongo.Pipeline{}, csOpts)
	if err != nil {
		cancel()
		return fmt.Errorf("open change stream: %w", err)
	}

	a.mu.Lock()
	a.watchCancel = cancel
	a.watchDone = make(chan struct{})
	a.watching = true
	a.mu.Unlock()

	// Rate-limited redraw loop: drains scheduleRedraw() signals and redraws
	// at most once every 100 ms to prevent flicker under high event rates.
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		pending := false
		for {
			select {
			case <-ctx.Done():
				if pending {
					a.redraw()
				}
				return
			case <-a.redrawCh:
				pending = true
			case <-ticker.C:
				if pending {
					a.redraw()
					pending = false
				}
			}
		}
	}()

	// Periodically re-enable pre-images to catch newly created collections
	// before we have a chance to process their "create" change event.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				enabler.EnableAll(ctx, a.client)
			}
		}
	}()

	// Main change stream goroutine.
	go func() {
		defer close(a.watchDone)
		defer cs.Close(context.Background())

		for cs.Next(ctx) {
			var event store.ChangeEntry
			if err := cs.Decode(&event); err != nil {
				a.addMessage("Decode error: %v", err)
				a.scheduleRedraw()
				continue
			}

			if enabler.IsSystem(event.NS.DB, event.NS.Coll) {
				continue
			}

			switch event.OperationType {
			case "create":
				if err := enabler.EnableCollection(ctx, a.client.Database(event.NS.DB), event.NS.Coll); err != nil {
					a.addMessage("Warning: pre-images on %s.%s: %v", event.NS.DB, event.NS.Coll, err)
				}
				// No scheduleRedraw — not a data event.
				continue
			case "insert", "update", "replace", "delete":
				// fall through
			default:
				continue
			}

			if event.OperationType != "insert" && len(event.FullDocumentBeforeChange) == 0 {
				a.addMessage("Warning: no pre-image for %s on %s.%s — skipping",
					event.OperationType, event.NS.DB, event.NS.Coll)
				a.scheduleRedraw()
				continue
			}

			a.mu.Lock()
			a.entries = append(a.entries, event)
			a.applyStats(event)
			a.mu.Unlock()

			if err := a.writer.Write(event); err != nil {
				a.addMessage("Log write error: %v", err)
			}

			a.scheduleRedraw()
		}

		if err := cs.Err(); err != nil && ctx.Err() == nil {
			a.addMessage("Change stream error: %v", err)
			a.scheduleRedraw()
		}
	}()

	return nil
}

func (a *App) stopWatching() {
	a.mu.Lock()
	if !a.watching {
		a.mu.Unlock()
		return
	}
	cancel := a.watchCancel
	done := a.watchDone
	a.mu.Unlock()

	cancel()
	<-done

	a.mu.Lock()
	a.watching = false
	a.mu.Unlock()
}

// ── actions ───────────────────────────────────────────────────────────────────

func (a *App) doRewind() {
	a.stopWatching()

	a.mu.Lock()
	snapshot := make([]store.ChangeEntry, len(a.entries))
	copy(snapshot, a.entries)
	a.mu.Unlock()

	if len(snapshot) == 0 {
		a.addMessage("Nothing to rewind.")
		if err := a.startWatching(); err != nil {
			a.addMessage("Error resuming watch: %v", err)
		}
		a.redraw()
		return
	}

	a.addMessage("Reverting %d operation(s)...", len(snapshot))
	a.redraw()

	r := reverter.New(a.client)
	if err := r.Rewind(context.Background(), snapshot); err != nil {
		a.addMessage("Rewind error: %v", err)
	} else {
		a.addMessage("Reverted: %s", rewindSummary(snapshot))

		a.mu.Lock()
		a.entries = nil
		a.stats = make(map[string]*collStats)
		a.mu.Unlock()

		a.logFile.Truncate(0)
		a.logFile.Seek(0, 0)
		a.writer = store.NewWriter(a.logFile)
	}

	if err := a.startWatching(); err != nil {
		a.addMessage("Error resuming watch: %v", err)
	}
	a.redraw()
}

func (a *App) clearLog() {
	a.mu.Lock()
	count := len(a.entries)
	a.entries = nil
	a.stats = make(map[string]*collStats)
	a.mu.Unlock()

	a.logFile.Truncate(0)
	a.logFile.Seek(0, 0)
	a.writer = store.NewWriter(a.logFile)

	a.addMessage("Cleared %d operation(s).", count)
}

// ── rendering ─────────────────────────────────────────────────────────────────

// scheduleRedraw signals that the screen needs updating. Non-blocking — the
// rate-limited goroutine will pick it up within 100 ms.
func (a *App) scheduleRedraw() {
	select {
	case a.redrawCh <- struct{}{}:
	default:
	}
}

// redraw takes an immediate snapshot and repaints the screen. Safe to call
// from any goroutine.
func (a *App) redraw() {
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || cols < 20 || rows < 5 {
		cols, rows = 80, 24
	}

	a.mu.Lock()
	ds := a.buildSnapshot(cols, rows)
	a.mu.Unlock()

	a.outMu.Lock()
	defer a.outMu.Unlock()
	drawScreen(ds)
}

// buildSnapshot must be called with a.mu held.
func (a *App) buildSnapshot(cols, rows int) displayState {
	statsCopy := make(map[string]collStats, len(a.stats))
	for k, v := range a.stats {
		statsCopy[k] = *v
	}
	return displayState{
		watching: a.watching,
		count:    len(a.entries),
		stats:    statsCopy,
		messages: append([]string{}, a.messages...),
		termCols: cols,
		termRows: rows,
	}
}

const (
	colWidth  = 28
	numWidth  = 9
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBold   = "\033[1m"
	ansiOff    = "\033[0m"
)

func drawScreen(ds displayState) {
	ruleLen := ds.termCols - 2
	if ruleLen < 10 {
		ruleLen = 10
	}
	rule := strings.Repeat("─", ruleLen)

	sb := &strings.Builder{}

	// ln writes one line, erasing to end-of-line, then CRLF.
	ln := func(format string, args ...interface{}) {
		s := fmt.Sprintf(format, args...)
		sb.WriteString(s)
		sb.WriteString("\033[K\r\n")
	}

	// Move to top-left, hide cursor — no full clear, we overwrite in place.
	sb.WriteString("\033[H\033[?25l")

	// ── fixed header ──────────────────────────────────────────────────────────
	ln("  %smongorewind%s", ansiBold, ansiOff)
	ln("  " + rule)

	status := fmt.Sprintf("%s○ idle%s", ansiYellow, ansiOff)
	if ds.watching {
		status = fmt.Sprintf("%s● watching%s", ansiGreen, ansiOff)
	}
	ln("  %s   %d operation(s) pending rewind", status, ds.count)
	ln("")

	// ── database summary table ────────────────────────────────────────────────
	// Aggregate per-collection stats into per-database totals.
	type dbRow struct {
		name                    string
		inserts, updates, deletes int
	}
	dbMap := make(map[string]*dbRow)
	for ns, s := range ds.stats {
		dot := strings.IndexByte(ns, '.')
		if dot < 0 {
			continue
		}
		db := ns[:dot]
		r := dbMap[db]
		if r == nil {
			r = &dbRow{name: db}
			dbMap[db] = r
		}
		r.inserts += s.inserts
		r.updates += s.updates
		r.deletes += s.deletes
	}

	dbs := make([]string, 0, len(dbMap))
	for db := range dbMap {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)

	// Header rows: header(5) + table-header(2) + totals(2) + messages(up to 5) = 14 fixed
	// Remaining rows available for database rows:
	maxDBRows := ds.termRows - 14
	if maxDBRows < 1 {
		maxDBRows = 1
	}

	if len(dbs) == 0 {
		ln("  No changes recorded.")
	} else {
		ln("  %s%-*s  %*s  %*s  %*s%s",
			ansiBold, colWidth, "Database",
			numWidth, "Inserts",
			numWidth, "Updates",
			numWidth, "Deletes",
			ansiOff)
		ln("  " + rule)

		shown := dbs
		hidden := 0
		if len(dbs) > maxDBRows {
			shown = dbs[:maxDBRows]
			hidden = len(dbs) - maxDBRows
		}

		var totIns, totUpd, totDel int
		for _, db := range shown {
			r := dbMap[db]
			ln("  %-*s  %*d  %*d  %*d",
				colWidth, r.name,
				numWidth, r.inserts,
				numWidth, r.updates,
				numWidth, r.deletes)
			totIns += r.inserts
			totUpd += r.updates
			totDel += r.deletes
		}
		// Add hidden rows to totals even if not displayed
		for _, db := range dbs[len(shown):] {
			r := dbMap[db]
			totIns += r.inserts
			totUpd += r.updates
			totDel += r.deletes
		}

		ln("  " + rule)
		ln("  %s%-*s  %*d  %*d  %*d%s",
			ansiBold, colWidth, "Total",
			numWidth, totIns,
			numWidth, totUpd,
			numWidth, totDel,
			ansiOff)

		if hidden > 0 {
			ln("  %s  (and %d more database(s) not shown)%s", ansiYellow, hidden, ansiOff)
		}
	}

	ln("")
	ln("  R=rewind  C=clear  Q=quit")

	// ── messages ──────────────────────────────────────────────────────────────
	if len(ds.messages) > 0 {
		ln("")
		for _, msg := range ds.messages {
			ln("  %s", msg)
		}
	}

	// Erase everything below the last line we wrote (handles shrinking content).
	sb.WriteString("\033[J")

	fmt.Print(sb.String())
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (a *App) applyStats(e store.ChangeEntry) {
	ns := e.NS.DB + "." + e.NS.Coll
	s := a.stats[ns]
	if s == nil {
		s = &collStats{}
		a.stats[ns] = s
	}
	switch e.OperationType {
	case "insert":
		s.inserts++
	case "update", "replace":
		s.updates++
	case "delete":
		s.deletes++
	}
}

func (a *App) addMessage(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	msg := fmt.Sprintf("[%s] %s", ts, fmt.Sprintf(format, args...))
	a.mu.Lock()
	a.messages = append(a.messages, msg)
	if len(a.messages) > 5 {
		a.messages = a.messages[len(a.messages)-5:]
	}
	a.mu.Unlock()
}

func rewindSummary(entries []store.ChangeEntry) string {
	dbMap := make(map[string]*collStats)
	for _, e := range entries {
		dot := strings.IndexByte(e.NS.DB, '.')
		db := e.NS.DB
		if dot >= 0 {
			db = e.NS.DB[:dot]
		}
		s := dbMap[db]
		if s == nil {
			s = &collStats{}
			dbMap[db] = s
		}
		switch e.OperationType {
		case "insert":
			s.inserts++
		case "update", "replace":
			s.updates++
		case "delete":
			s.deletes++
		}
	}

	dbs := make([]string, 0, len(dbMap))
	for db := range dbMap {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)

	parts := make([]string, 0, len(dbs))
	for _, db := range dbs {
		s := dbMap[db]
		parts = append(parts, fmt.Sprintf("%s (%d ins, %d upd, %d del)", db, s.inserts, s.updates, s.deletes))
	}
	return strings.Join(parts, " | ")
}
