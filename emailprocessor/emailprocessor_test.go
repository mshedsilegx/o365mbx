package emailprocessor

import (
	"os"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func TestEmailProcessor_IsHTML(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"Plain text", "Hello world", false},
		{"Simple HTML", "<html><body>Hello</body></html>", true},
		{"HTML tag", "Check this <br> tag", true},
		{"Self-closing tag", "Link <img src='foo.png' />", true},
		{"Uppercase tag", "<P>Hello</P>", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ep.IsHTML(tt.content))
		})
	}
}

func TestEmailProcessor_CleanHTML(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	htmlContent := `
		<html>
			<head><style>body { color: red; }</style></head>
			<body>
				<h1>Title</h1>
				<p>This is a paragraph with a <a href="https://example.com">link</a>.</p>
				<ul>
					<li>Item 1</li>
					<li>Item 2</li>
				</ul>
				<img src="img.png" alt="An image">
				<script>alert('hidden');</script>
			</body>
		</html>
	`

	clean, err := ep.CleanHTML(htmlContent)
	assert.NoError(t, err)
	assert.Contains(t, clean, "Title")
	assert.Contains(t, clean, "This is a paragraph")
	assert.Contains(t, clean, "[link](https://example.com)")
	assert.Contains(t, clean, "Item 1")
	assert.Contains(t, clean, "[Image: An image]")
	assert.NotContains(t, clean, "alert('hidden')")
	assert.NotContains(t, clean, "color: red")
}

func TestEmailProcessor_ProcessBody(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	html := "<b>Bold</b>"

	// Test "none"
	res, err := ep.ProcessBody(html, "none", "")
	assert.NoError(t, err)
	assert.Equal(t, html, res)

	// Test "text"
	res, err = ep.ProcessBody(html, "text", "")
	assert.NoError(t, err)
	assert.Equal(t, "Bold", res)

	// Test invalid
	res, err = ep.ProcessBody(html, "invalid", "")
	assert.Error(t, err)
	assert.Nil(t, res)

	// Test "pdf" with invalid path (to cover the switch case without launching browser)
	res, err = ep.ProcessBody(html, "pdf", "invalid/path/to/chromium")
	assert.Error(t, err)
	assert.Nil(t, res)
}

func TestEmailProcessor_ConvertToPDF(t *testing.T) {
	chromiumPath := os.Getenv("PDF_TEST_CHROMIUM_PATH")
	if chromiumPath == "" {
		t.Skip("Skipping PDF conversion test: PDF_TEST_CHROMIUM_PATH environment variable not set")
	}

	if _, err := os.Stat(chromiumPath); os.IsNotExist(err) {
		t.Fatalf("PDF_TEST_CHROMIUM_PATH set but file does not exist: %s", chromiumPath)
	}

	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	html := "<html><body><h1>Test PDF</h1></body></html>"
	pdfBytes, err := ep.ConvertToPDF(html, chromiumPath)

	assert.NoError(t, err)
	assert.NotNil(t, pdfBytes)
	assert.True(t, len(pdfBytes) > 0)

	// Verify PDF header
	if len(pdfBytes) >= 4 {
		assert.Equal(t, "%PDF", string(pdfBytes[:4]))
	}
}

func TestEmailProcessor_CleanHTML_EdgeCases(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	// Test empty content
	res, err := ep.CleanHTML("")
	assert.NoError(t, err)
	assert.Empty(t, res)

	// Test link with no text
	res, err = ep.CleanHTML("<a href='http://foo.com'></a>")
	assert.NoError(t, err)
	assert.Contains(t, res, "[http://foo.com](http://foo.com)")

	// Test entity unescaping
	res, err = ep.CleanHTML("&lt;tag&gt; &amp; &quot;quote&quot;")
	assert.NoError(t, err)
	assert.Equal(t, "<tag> & \"quote\"", res)
}
