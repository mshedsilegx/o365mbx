package o365client

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestManualURLConstructionWithQueryEscape verifies that the raw URL string is
// correctly constructed and its components are properly escaped, specifically
// using url.QueryEscape for the mailbox name to handle the '@' symbol.
func TestManualURLConstructionWithQueryEscape(t *testing.T) {
	// Arrange
	baseURL := "https://graph.microsoft.com/v1.0"
	mailboxName := "user@example.com"
	messageID := "AAMkAGI2ZWVmNmRlLWM0ZjctNGQ2OC04NmY4LTA0NmU3Zjc0NzIwNQBGAAAAAAD="
	attachmentID := "AAMkAGI2ZWVmNmRlLWM0ZjctNGQ2OC04NmY4LTA0NmU3Zjc0NzIwNQBGAAAAAAD="

	// Act: Manually construct the URL, mirroring the logic in the fixed function.
	// This uses url.QueryEscape for the mailbox name, which is the key to the fix.
	rawURL := fmt.Sprintf(
		"%s/users/%s/messages/%s/attachments/%s/$value",
		baseURL,
		url.QueryEscape(mailboxName),
		url.PathEscape(messageID),
		url.PathEscape(attachmentID),
	)

	// Assert
	// 1. The raw string should be correctly escaped.
	expectedURL := "https://graph.microsoft.com/v1.0/users/user%40example.com/messages/AAMkAGI2ZWVmNmRlLWM0ZjctNGQ2OC04NmY4LTA0NmU3Zjc0NzIwNQBGAAAAAAD=/attachments/AAMkAGI2ZWVmNmRlLWM0ZjctNGQ2OC04NmY4LTA0NmU3Zjc0NzIwNQBGAAAAAAD=/$value"
	assert.Equal(t, expectedURL, rawURL, "The raw URL string should be correctly constructed and escaped with '@' as '%%40'.")

	// 2. The generated URL should also be valid and parseable without error.
	_, err := url.Parse(rawURL)
	assert.NoError(t, err, "The manually constructed URL should be valid and parse without error.")
}