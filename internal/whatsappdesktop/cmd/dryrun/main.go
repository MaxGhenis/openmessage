// dryrun opens only WhatsApp Desktop's source database, read-only. It never
// opens an OpenMessage store or transport and prints only aggregate counts.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/maxghenis/openmessage/internal/whatsappdesktop"
)

func main() {
	dbPath := flag.String("db", whatsappdesktop.DefaultDBPath(), "Desktop ChatStorage.sqlite path")
	sinceMS := flag.Int64("since-ms", 1787260699000, "timestamp for aggregate recent-row counts")
	flag.Parse()
	var chunks, messages, localAttachments int64
	stats, err := whatsappdesktop.New(whatsappdesktop.Options{DBPath: *dbPath}).Scan(context.Background(), whatsappdesktop.ScanOptions{SinceMS: *sinceMS, ChunkSize: 100}, func(chunk whatsappdesktop.Chunk) error {
		chunks++
		messages += int64(len(chunk.Messages))
		for _, message := range chunk.Messages {
			if message.LocalMediaRef != "" {
				localAttachments++
			}
		}
		return nil
	})
	if err != nil {
		// Source errors may contain path or corrupt column values; keep this
		// count-only diagnostic's output free of any personal source values.
		fmt.Fprintln(os.Stderr, "Desktop read-only scan failed")
		os.Exit(1)
	}
	output := struct {
		whatsappdesktop.Stats
		Chunks           int64 `json:"chunks"`
		Messages         int64 `json:"messages"`
		LocalAttachments int64 `json:"local_attachments"`
	}{stats, chunks, messages, localAttachments}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		os.Exit(1)
	}
}
