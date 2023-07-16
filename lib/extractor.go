package lib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/PuerkitoBio/goquery"
	"github.com/k3a/html2text"
)

type RawPost struct {
	str string
}

func (r *RawPost) ToPost() (Post, error) {
	var wrapper PostWrapper
	err := json.Unmarshal([]byte(r.str), &wrapper)
	if err != nil {
		return Post{}, err

	}
	return wrapper.Post, nil
}

type Post struct {
	Id               int    `json:"id"`
	PublicationId    int    `json:"publication_id"`
	Type             string `json:"type"`
	Slug             string `json:"slug"`
	PostDate         string `json:"post_date"`
	CanonicalUrl     string `json:"canonical_url"`
	PreviousPostSlug string `json:"previous_post_slug"`
	NextPostSlug     string `json:"next_post_slug"`
	CoverImage       string `json:"cover_image"`
	Description      string `json:"description"`
	WordCount        int    `json:"wordcount"`
	//PostTags         []string `json:"postTags"`
	Title    string `json:"title"`
	BodyHTML string `json:"body_html"`
}

func (p *Post) ToMD(withTitle bool) (string, error) {
	var title string
	if withTitle {
		title = fmt.Sprintf("# %s\n\n", p.Title)
	}
	converter := md.NewConverter("", true, nil)
	body, err := converter.ConvertString(p.BodyHTML)
	if err != nil {
		return "", err
	}
	return title + body, nil
}

func (p *Post) ToText(withTitle bool) string {
	if withTitle {
		return p.Title + "\n\n" + html2text.HTML2Text(p.BodyHTML)
	}
	return html2text.HTML2Text(p.BodyHTML)
}

func (p *Post) ToHTML(withTitle bool) string {
	if withTitle {
		return fmt.Sprintf("<h1>%s</h1>\n\n%s", p.Title, p.BodyHTML)
	}
	return p.BodyHTML
}

func (p *Post) ToJSON() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (p *Post) WriteToFile(path string, format string) error {
	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var content string
	switch format {
	case "html":
		content = p.ToHTML(true)
	case "md":
		content, err = p.ToMD(true)
		if err != nil {
			return err
		}
	case "txt":
		content = p.ToText(true)
	default:
		return fmt.Errorf("unknown format: %s", format)
	}
	_, err = f.WriteString(content)
	if err != nil {
		return err
	}

	err = f.Sync()
	if err != nil {
		return err
	}

	return nil
}

type PostWrapper struct {
	Post Post `json:"post"`
}

type Extractor struct {
	fetcher *Fetcher
}

func NewExtractor(f *Fetcher) *Extractor {
	if f == nil {
		f = NewFetcher(10, nil)
	}
	return &Extractor{fetcher: f}
}

func findScriptContent(doc *goquery.Document) string {
	scriptContent := ""
	doc.Find("script").EachWithBreak(func(i int, s *goquery.Selection) bool {
		if strings.Contains(s.Text(), "window._preloads") && strings.Contains(s.Text(), "JSON.parse(") {
			scriptContent = s.Text()
			return false
		}
		return true
	})
	return scriptContent
}

func extractJSONString(scriptContent string) (string, error) {
	start := strings.Index(scriptContent, "JSON.parse(\"")
	end := strings.LastIndex(scriptContent, "\")")

	if start == -1 || end == -1 || start >= end {
		return "", errors.New("failed to extract JSON string")
	}

	return scriptContent[start+len("JSON.parse(\"") : end], nil
}

func (e *Extractor) ExtractPost(ctx context.Context, pageUrl string) (Post, error) {
	// fetch page HTML content
	body, err := e.fetcher.FetchURL(ctx, pageUrl)
	if err != nil {
		return Post{}, fmt.Errorf("failed to fetch page: %s", err)
	}
	defer body.Close()

	doc, err := goquery.NewDocumentFromReader(body)
	if err != nil {
		return Post{}, fmt.Errorf("failed to fetch page: %s", err)

	}

	scriptContent := findScriptContent(doc)

	if scriptContent == "" {
		return Post{}, fmt.Errorf("failed to fetch page: script content not found")
	}

	jsonString, err := extractJSONString(scriptContent)
	if err != nil {
		return Post{}, fmt.Errorf("failed to fetch page: %s", err)
	}

	// jsonString is a stringified JSON string. Convert it to a normal JSON string
	var rawJSON RawPost
	err = json.Unmarshal([]byte("\""+jsonString+"\""), &rawJSON.str) //json.NewEncoder(&rawJSON).Encode([]byte("\"" + jsonString + "\""))
	if err != nil {
		return Post{}, fmt.Errorf("failed to fetch page: %s", err)
	}

	// Now convert the normal JSON string to a Go object
	p, err := rawJSON.ToPost()
	if err != nil {
		return Post{}, fmt.Errorf("failed to fetch page: %s", err)
	}

	return p, nil
}

func (e *Extractor) GetAllPostsURLs(ctx context.Context, pubUrl string) ([]string, error) {
	u, err := url.Parse(pubUrl)
	if err != nil {
		return nil, err
	}

	u.Path, err = url.JoinPath(u.Path, "sitemap.xml")
	if err != nil {
		return nil, err
	}

	// fetch the sitemap of the publication
	body, err := e.fetcher.FetchURL(ctx, u.String())
	if err != nil {
		return nil, err
	}
	defer body.Close()
	// the sitemap is an XML file with a list of URLs
	// we are interested in the <loc> tags only if the URL contains "/p/"
	doc, err := goquery.NewDocumentFromReader(body)
	if err != nil {
		return nil, err
	}

	urls := []string{}
	doc.Find("loc").EachWithBreak(func(i int, s *goquery.Selection) bool {
		// Check if the context has been cancelled
		select {
		case <-ctx.Done():
			return false
		default:
		}
		url := s.Text()
		if strings.Contains(url, "/p/") {
			urls = append(urls, url)
		}

		return true
	})

	return urls, nil
}

type ExtractResult struct {
	Post Post
	Err  error
}

func (e *Extractor) ExtractAllPosts(ctx context.Context, urls []string) <-chan ExtractResult {
	ch := make(chan ExtractResult, len(urls))

	go func() {
		var wg sync.WaitGroup
		wg.Add(len(urls))
		for _, u := range urls {
			go func(url string) {
				defer wg.Done()
				post, err := e.ExtractPost(ctx, url)
				ch <- ExtractResult{Post: post, Err: err}
			}(u)
		}
		wg.Wait()
		close(ch)
	}()

	return ch
}
