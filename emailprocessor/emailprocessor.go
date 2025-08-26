package emailprocessor

import (
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

var spaceRegex = regexp.MustCompile(`\s+`)

type EmailProcessor struct{}

func NewEmailProcessor() *EmailProcessor {
	return &EmailProcessor{}
}

// unescapeHTML unescapes common HTML entities.
func unescapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	// Add more entities as needed
	return s
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
			linkTextBuilder := strings.Builder{}
			var extractLinkText func(*html.Node)
			extractLinkText = func(node *html.Node) {
				if node.Type == html.TextNode {
					linkTextBuilder.WriteString(node.Data)
				}
				for c := node.FirstChild; c != nil; c = c.NextSibling {
					extractLinkText(c)
				}
			}
			extractLinkText(n)
			linkText := strings.TrimSpace(linkTextBuilder.String())

			if linkText != "" && href != "" {
				sb.WriteString(fmt.Sprintf(" [%s](%s) ", linkText, href))
			} else if href != "" {
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
			text := unescapeHTML(n.Data) // Unescape HTML entities
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
	plainText = strings.ReplaceAll(plainText, "\r\n", "\n")                      // Normalize line endings
	plainText = strings.ReplaceAll(plainText, "\r", "\n")                        // Normalize line endings
	plainText = regexp.MustCompile(`\\n{2,}`).ReplaceAllString(plainText, "\n\n") // Consolidate multiple newlines
	plainText = spaceRegex.ReplaceAllString(plainText, " ")                      // Consolidate multiple spaces
	plainText = strings.TrimSpace(plainText)

	return plainText, nil
}
