// Package presenter provides formatted output and display logic for health checks
// and message details.
//
// OBJECTIVE:
// This package is responsible for the "Presentation" layer of the application. It takes
// raw data structures from the engine or client and formats them into human-readable
// tables and summaries for the console.
//
// CORE FUNCTIONALITY:
//  1. Health Check Display: Formats mailbox-level statistics and folder lists into
//     aligned tables using 'text/tabwriter'.
//  2. Message Detail Streaming: Streams and displays metadata for individual messages
//     within a folder, providing real-time feedback during diagnostic runs.
package presenter

import (
	"context"
	"fmt"
	"io"
	"o365mbx/o365client"
	"text/tabwriter"

	log "github.com/sirupsen/logrus"
)

// RunHealthCheckMode executes a diagnostic run to display mailbox statistics.
// It retrieves aggregate stats (total messages, total size) and a sorted list
// of all folders with their individual item counts and storage sizes.
func RunHealthCheckMode(ctx context.Context, client o365client.O365ClientInterface, mailboxName string, out io.Writer) error {
	log.WithField("mailbox", mailboxName).Info("Performing health check...")
	stats, err := client.GetMailboxHealthCheck(ctx, mailboxName)
	if err != nil {
		return fmt.Errorf("O365 API error during health check: %w", err)
	}

	log.WithField("mailbox", mailboxName).Info("Health check successful.")
	_, _ = fmt.Fprintln(out, "\n--- Mailbox Health Check ---")
	_, _ = fmt.Fprintf(out, "Mailbox: %s\n", mailboxName)
	_, _ = fmt.Fprintln(out, "------------------------------")
	_, _ = fmt.Fprintf(out, "Total Messages: %d\n", stats.TotalMessages)
	_, _ = fmt.Fprintf(out, "Total Folders: %d\n", len(stats.Folders))
	_, _ = fmt.Fprintf(out, "Total Mailbox Size: %.2f MB\n", float64(stats.TotalMailboxSize)/1024/1024)
	_, _ = fmt.Fprintln(out, "------------------------------")
	_, _ = fmt.Fprintln(out, "\n--- Folder Statistics ---")

	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', tabwriter.AlignRight)
	if _, err := fmt.Fprintln(w, "Folder\tItems\tSize (KB)\t"); err != nil {
		log.Warnf("Error writing to tabwriter: %v", err)
	}
	if _, err := fmt.Fprintln(w, "-------\t-----\t---------\t"); err != nil {
		log.Warnf("Error writing to tabwriter: %v", err)
	}

	for _, folder := range stats.Folders {
		folderSizeKB := float64(folder.Size) / 1024
		if _, err := fmt.Fprintf(w, "%s\t%d\t%.2f\t\n", folder.Name, folder.TotalItems, folderSizeKB); err != nil {
			log.Warnf("Error writing to tabwriter: %v", err)
		}
	}

	if err := w.Flush(); err != nil {
		log.Warnf("Error flushing tabwriter: %v", err)
	}
	_, _ = fmt.Fprintln(out, "-------------------------")
	return nil
}

// RunMessageDetailsMode streams and displays metadata for all messages in a specific folder.
// It provides a granular view of message attributes including sender, recipients,
// date, subject, and attachment details (count and total size).
func RunMessageDetailsMode(ctx context.Context, client o365client.O365ClientInterface, mailboxName, folderName string, out io.Writer) error {
	log.WithFields(log.Fields{
		"mailbox": mailboxName,
		"folder":  folderName,
	}).Info("Streaming message details for folder...")

	detailsChan := make(chan o365client.MessageDetail)
	errChan := make(chan error, 1)

	go func() {
		errChan <- client.GetMessageDetailsForFolder(ctx, mailboxName, folderName, detailsChan)
	}()

	_, _ = fmt.Fprintf(out, "\n--- Message Details for Folder: %s ---\n", folderName)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "From\tTo\tDate\tSubject\tAttachments\tTotal Size (KB)\t"); err != nil {
		log.Warnf("Error writing to tabwriter: %v", err)
	}
	if _, err := fmt.Fprintln(w, "----\t--\t----\t-------\t-----------\t----------------\t"); err != nil {
		log.Warnf("Error writing to tabwriter: %v", err)
	}

	// Loop will end when detailsChan is closed by the client
	for msg := range detailsChan {
		from := msg.From
		to := msg.To
		subject := msg.Subject
		if len(subject) > 75 {
			subject = subject[:72] + "..."
		}
		totalSizeKB := float64(msg.AttachmentsTotalSize) / 1024

		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%.2f\t\n",
			from,
			to,
			msg.Date.Format("2006-01-02 15:04"),
			subject,
			msg.AttachmentCount,
			totalSizeKB,
		); err != nil {
			log.Warnf("Error writing to tabwriter: %v", err)
		}
	}

	if err := w.Flush(); err != nil {
		log.Warnf("Error flushing tabwriter: %v", err)
	}
	_, _ = fmt.Fprintln(out, "-------------------------------------------------")

	// Check for errors from the goroutine
	if err := <-errChan; err != nil {
		return fmt.Errorf("failed to get message details: %w", err)
	}
	return nil
}
