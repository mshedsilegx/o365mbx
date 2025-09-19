package emailprocessor

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"golang.org/x/net/html"
)

var spaceRegex = regexp.MustCompile(`\s+`)

type EmailProcessor struct{}

func NewEmailProcessor() *EmailProcessor {
	return &EmailProcessor{}
}

// ProcessBody converts the body of an email to the specified format.
func (ep *EmailProcessor) ProcessBody(htmlContent, convertBody, chromiumPath string) (interface{}, error) {
	switch convertBody {
	case "none":
		return htmlContent, nil
	case "text":
		return ep.CleanHTML(htmlContent)
	case "pdf":
		return ep.ConvertToPDF(htmlContent, chromiumPath)
	default:
		return nil, fmt.Errorf("invalid convertBody value: %s", convertBody)
	}
}

// ConvertToPDF converts HTML content to PDF using go-rod.
func (ep *EmailProcessor) ConvertToPDF(htmlContent, chromiumPath string) ([]byte, error) {
	l := launcher.New().Bin(chromiumPath)
	defer l.Cleanup()

	browserURL, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("failed to launch browser: %w", err)
	}

	browser := rod.New().ControlURL(browserURL).MustConnect()
	defer browser.MustClose()

	page := browser.MustPage()
	page.MustNavigate("data:text/html," + htmlContent)
	pdf, err := page.PDF(&proto.PagePrintToPDF{})
	if err != nil {
		return nil, fmt.Errorf("failed to generate PDF: %w", err)
	}
	defer pdf.Close()

	pdfBytes, err := io.ReadAll(pdf)
	if err != nil {
		return nil, fmt.Errorf("failed to read PDF stream: %w", err)
	}

	return pdfBytes, nil
}

// CleanHTML converts HTML content to plain text and removes special characters.
func (ep *EmailProcessor) CleanHTML(htmlContent string) (string, error) {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return "", fmt.Errorf("failed to parse HTML: %w", err)
	}

	var sb strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		// Skip script and style tags
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}

		// Handle links
		if n.Type == html.ElementNode && n.Data == "a" {
			var href string
			for _, attr := range n.Attr {
				if attr.Key == "href" {
					href = attr.Val
					break
				}
			}
			// Extract link text
			var linkTextBuilder strings.Builder
			var extractText func(*html.Node)
			extractText = func(node *html.Node) {
				if node.Type == html.TextNode {
					linkTextBuilder.WriteString(node.Data)
				}
				for c := node.FirstChild; c != nil; c = c.NextSibling {
					extractText(c)
				}
			}
			extractText(n)
			linkText := strings.TrimSpace(linkTextBuilder.String())

			// If link text is empty, try to use the href as the text
			if linkText == "" {
				linkText = href
			}

			if linkText != "" && href != "" {
				sb.WriteString(fmt.Sprintf(" [%s](%s) ", linkText, href))
			} else if href != "" { // Should be rare due to the fallback above
				sb.WriteString(fmt.Sprintf(" %s ", href))
			}
			return // Skip processing children as we've handled the link
		}

		// Handle images
		if n.Type == html.ElementNode && n.Data == "img" {
			var altText string
			for _, attr := range n.Attr {
				if attr.Key == "alt" {
					altText = attr.Val
					break
				}
			}
			if altText != "" {
				sb.WriteString(fmt.Sprintf(" [Image: %s] ", altText))
			}
			return // Skip processing children as we've handled the image
		}

		// Add newlines for block-level elements
		isBlock := func(node *html.Node) bool {
			switch node.Data {
			case "address", "article", "aside", "blockquote", "details", "dialog", "dd", "div", "dl", "dt",
				"fieldset", "figcaption", "figure", "footer", "form", "h1", "h2", "h3", "h4", "h5", "h6",
				"header", "hgroup", "hr", "li", "main", "nav", "ol", "p", "pre", "section", "table",
				"ul":
				return true
			default:
				return false
			}
		}

		if n.Type == html.ElementNode && isBlock(n) {
			if sb.Len() > 0 && sb.String()[sb.Len()-1] != '\n' {
				sb.WriteString("\n")
			}
		}

		if n.Type == html.TextNode {
			text := html.UnescapeString(n.Data) // Unescape HTML entities
			text = strings.TrimSpace(text)
			if text != "" {
				// Add a space before the text if the builder is not empty and the last char is not a space or newline
				lastChar := ' ' // Initialize with a space to avoid adding space at the very beginning
				if sb.Len() > 0 {
					lastChar = rune(sb.String()[sb.Len()-1])
				}
				if lastChar != ' ' && lastChar != '\n' {
					sb.WriteString(" ")
				}
				sb.WriteString(text)
			}
		}

		// Recursively traverse child nodes
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}

		if n.Type == html.ElementNode && isBlock(n) {
			if sb.Len() > 0 && sb.String()[sb.Len()-1] != '\n' {
				sb.WriteString("\n")
			}
		}
	}
	f(doc)

	plainText := sb.String()

	// Consolidate multiple newlines and spaces
	plainText = strings.ReplaceAll(plainText, "\r\n", "\n")                       // Normalize line endings
	plainText = strings.ReplaceAll(plainText, "\r", "\n")                         // Normalize line endings
	plainText = regexp.MustCompile(`\\n{2,}`).ReplaceAllString(plainText, "\n\n") // Consolidate multiple newlines
	plainText = spaceRegex.ReplaceAllString(plainText, " ")                       // Consolidate multiple spaces
	plainText = strings.TrimSpace(plainText)

	return plainText, nil
}
