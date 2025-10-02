package presenter

import (
	"context"
	"fmt"
	"o365mbx/o365client"
	"os"
	"text/tabwriter"

	log "github.com/sirupsen/logrus"
)

func RunHealthCheckMode(ctx context.Context, client o365client.O365ClientInterface, mailboxName string) {
	log.WithField("mailbox", mailboxName).Info("Performing health check...")
	stats, err := client.GetMailboxHealthCheck(ctx, mailboxName)
	if err != nil {
		log.Fatalf("O365 API error during health check: %v", err)
	}

	log.WithField("mailbox", mailboxName).Info("Health check successful.")
	fmt.Println("\n--- Mailbox Health Check ---")
	fmt.Printf("Mailbox: %s\n", mailboxName)
	fmt.Println("------------------------------")
	fmt.Printf("Total Messages: %d\n", stats.TotalMessages)
	fmt.Printf("Total Folders: %d\n", len(stats.Folders))
	fmt.Printf("Total Mailbox Size: %.2f MB\n", float64(stats.TotalMailboxSize)/1024/1024)
	fmt.Println("------------------------------")
	fmt.Println("\n--- Folder Statistics ---")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', tabwriter.AlignRight)
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
	fmt.Println("-------------------------")
}

func RunMessageDetailsMode(ctx context.Context, client o365client.O365ClientInterface, mailboxName, folderName string) {
	log.WithFields(log.Fields{
		"mailbox": mailboxName,
		"folder":  folderName,
	}).Info("Streaming message details for folder...")

	detailsChan := make(chan o365client.MessageDetail)
	errChan := make(chan error, 1)

	go func() {
		errChan <- client.GetMessageDetailsForFolder(ctx, mailboxName, folderName, detailsChan)
	}()

	fmt.Printf("\n--- Message Details for Folder: %s ---\n", folderName)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
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
	fmt.Println("-------------------------------------------------")

	// Check for errors from the goroutine
	if err := <-errChan; err != nil {
		log.Fatalf("Failed to get message details: %v", err)
	}
}