package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"
	"github.com/gocolly/colly/v2"
)

// Configuration holds all the data passed from the config file
// the target is specified in a flag so we don't have to edit the configuration file every time we run the tool
type Configuration struct {
	Headers             map[string]string
	Depth               int
	LogCrawledURLs      bool
	LogQueries          map[string]string
	LogURLRegex         []string
	LogNon200Queries    map[string]string
	ExcludedURLRegex    []string
	ExcludedStatusCodes []int
	LogInlineJS         bool
}

type job struct {
	URL                 string
	Headers             map[string]string
	Depth               int
	LogQueries          map[string]string
	LogURLRegex         []string
	LogNon200Queries    map[string]string
	ExcludedURLRegex    []string
	ExcludedStatusCodes []int
	LogInlineJS         bool
}

// global variables to store the gathered info
var loggedQueries = struct {
	sync.RWMutex
	content map[string][]string
}{content: make(map[string][]string)}

var loggedNon200Queries = struct {
	sync.RWMutex
	content map[string][]string
}{content: make(map[string][]string)}

var loggedInlineJS = struct {
	sync.RWMutex
	content map[string][]string
}{content: make(map[string][]string)}

var (
	target     = flag.String("target", "http://127.0.0.1", "Target URL")
	configFile = flag.String("config", "config.json", "Configuration file")
	outdir     = flag.String("output", "output", "Directory to save results in")
	debug      = flag.Bool("debug", false, "Print visited links in real-time to stdout")
	insecure   = flag.Bool("insecure", false, "Accept untrusted SSL/TLS certificates")
	depth      = flag.Int("depth", 2, "Depth to crawl")
	threads    = flag.Int("threads", 10, "Number of threads")
)

// store configuration in a global variable accessible to all functions so we don't have to pass it around all the time
var config Configuration

func main() {
	flag.Parse()

	config, err := getConfigFile(*configFile)
	if err != nil {
		log.Fatal(err)
	}

	hostname, err := getHostname(*target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Target URL is invalid: %v", err)
		os.Exit(1)
	}

	// Instantiate default collector
	c := colly.NewCollector(
		colly.MaxDepth(*depth),
		colly.Async(),
	)
	c.Limit(&colly.LimitRule{DomainGlob: "*", Parallelism: *threads})

	// Allow URLs from the same domain and its subdomains
	c.URLFilters = []*regexp.Regexp{
		regexp.MustCompile(".*" + strings.ReplaceAll(hostname, ".", "\\.") + ".*"),
	}

	// Add headers
	c.OnRequest(func(r *colly.Request) {
		for header, value := range config.Headers {
			r.Headers.Set(header, value)
		}
	})

	// Accept untrusted SSL/TLS certificates based on the value of `-insecure` flag
	c.WithTransport(&http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure},
	})

	// On every a element which has href attribute call callback
	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		link := e.Attr("href")

		// Print link if it's in-scope
		if checkOrigin(link, *target) {
			fmt.Println(link)
		}

		// Visit link found on page on a new thread
		e.Request.Visit(link)
	})

	// Start scraping
	c.Visit(*target)
	// Wait until threads are finished
	c.Wait()

	os.MkdirAll(*outdir, os.ModePerm)

	if config.LogQueries != nil {
		err = writeResults("logged-queries.json", loggedQueries.content)
		if err != nil {
			log.Printf("Error writing query results: %v", err)
		}
	}
	if config.LogInlineJS {
		err = writeResults("inline-scripts.json", loggedInlineJS.content)
		if err != nil {
			log.Printf("Error writing inline scripts: %v", err)
		}
	}
	if config.LogNon200Queries != nil {
		err = writeResults("logged-non-200-queries.json", loggedNon200Queries.content)
		if err != nil {
			log.Printf("Error writing non-200 query results: %v", err)
		}
	}
}

func getConfigFile(location string) (Configuration, error) {
	f, err := os.Open(location)
	if err != nil {
		return Configuration{}, fmt.Errorf("could not open Configuration file: %v", err)
	}
	defer f.Close()

	decoder := json.NewDecoder(f)
	config := Configuration{}
	err = decoder.Decode(&config)
	if err != nil {
		return Configuration{}, fmt.Errorf("could not decode Configuration file: %v", err)
	}

	return config, nil
}

func crawl(j job, q chan job, wg *sync.WaitGroup) {
	defer wg.Done()

	res, err := httpGET(j.URL, j.Headers)
	if err != nil {
		log.Print(err)
		return
	}

	if res.StatusCode == http.StatusTooManyRequests {
		log.Printf("you are being rate limited")
		return
	}

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		log.Printf("could not parse page: %v", err)
		return
	}
	res.Body.Close()

	if j.LogQueries != nil {
		var foundResources []string
		for t, a := range j.LogQueries {
			resources := attrScrape(t, a, doc)
			if j.LogURLRegex != nil {
				resources = matchURLRegex(resources, j.LogURLRegex)
			}
			foundResources = append(foundResources, resources...)
		}

		if len(foundResources) > 0 {
			loggedQueries.Lock()
			loggedQueries.content[j.URL] = foundResources
			loggedQueries.Unlock()
		}
	}

	if j.LogInlineJS {
		inlineScriptCode := scrapeScripts(doc)

		if len(inlineScriptCode) > 0 {
			loggedInlineJS.Lock()
			loggedInlineJS.content[j.URL] = inlineScriptCode
			loggedInlineJS.Unlock()
		}
	}

	if j.LogNon200Queries != nil {
		var foundResources []string
		for t, a := range j.LogNon200Queries {
			links := attrScrape(t, a, doc)
			for _, link := range links {
				absolute, _ := absURL(link, j.URL)
				if isNon200(absolute, j.Headers, j.ExcludedStatusCodes, j.ExcludedURLRegex) {
					foundResources = append(foundResources, absolute)
				}
			}
		}

		if len(foundResources) > 0 {
			loggedNon200Queries.Lock()
			loggedNon200Queries.content[j.URL] = foundResources
			loggedNon200Queries.Unlock()
		}
	}

	urls := attrScrape("a", "href", doc)
	tovisit := toVisit(urls, j.URL, j.ExcludedURLRegex)

	if *debug {
		fmt.Println(j.URL)
	}

	if j.Depth <= 1 {
		return
	}

	wg.Add(len(tovisit))
	for _, u := range tovisit {
		q <- job{u, j.Headers, j.Depth - 1, j.LogQueries, j.LogURLRegex, j.LogNon200Queries, j.ExcludedURLRegex, j.ExcludedStatusCodes, j.LogInlineJS}
	}
}

func httpGET(url string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("could not create request for %s: %v", url, err)
	}

	for key, value := range headers {
		req.Header.Add(key, value)
	}

	client := &http.Client{}

	if *insecure {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client = &http.Client{Transport: tr}
	}

	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not request %s: %v", url, err)
	}
	return res, nil
}

func writeResults(filename string, content map[string][]string) error {
	JSON, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("could not marshal the JSON object: %v", err)
	}
	err = ioutil.WriteFile(filepath.Join(*outdir, filename), JSON, 0644)
	if err != nil {
		return fmt.Errorf("coudln't write resources to JSON: %v", err)
	}
	return nil
}

func attrScrape(tag string, attr string, doc *goquery.Document) []string {
	var results []string
	doc.Find(tag).Each(func(index int, tag *goquery.Selection) {
		attr, exists := tag.Attr(attr)
		if exists {
			results = append(results, attr)
		}
	})
	return results
}

func scrapeScripts(doc *goquery.Document) []string {
	var inlineScripts []string

	doc.Find("script").Each(func(index int, tag *goquery.Selection) {
		// check if the tag does not have a src attribute
		// if it doesn't, assume it's an inline script
		_, exists := tag.Attr("src")
		if !exists {
			inlineScripts = append(inlineScripts, tag.Text())
		}
	})

	return inlineScripts
}

func getHostname(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return u.Hostname(), nil
}

func checkOrigin(link, base string) bool {
	linkurl, err := url.Parse(link)
	if err != nil {
		return false
	}

	linkhost := linkurl.Hostname()

	baseURL, err := url.Parse(base)
	if err != nil {
		return false
	}
	basehost := baseURL.Hostname()

	// check the main domain not the subdomain
	// checkOrigin ("https://docs.google.com", "https://mail.google.com") => true
	re, _ := regexp.Compile("[\\w-]*\\.[\\w]*$")
	if re.FindString(linkhost) == re.FindString(basehost) {
		return true
	}
	return false
}

func absURL(href, base string) (string, error) {
	url, err := url.Parse(href)
	if err != nil {
		return "", fmt.Errorf("couldn't parse URL: %v", err)
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("couldn't parse URL: %v", err)
	}
	url = baseURL.ResolveReference(url)
	return url.String(), nil
}

func toVisit(urls []string, base string, excludedRegex []string) []string {
	var tovisit []string
	for _, u := range urls {
		absolute, err := absURL(u, base)
		if err != nil {
			log.Printf("couldn't parse URL: %v", err)
			continue
		}
		if !(strings.HasPrefix(absolute, "http://") || strings.HasPrefix(absolute, "https://")) {
			continue
		}
		if matchURLRegexLink(u, excludedRegex) {
			continue
		}
		if checkOrigin(absolute, base) {
			tovisit = append(tovisit, absolute)
		}
	}
	return tovisit
}

func matchURLRegexLink(link string, regex []string) bool {
	for _, re := range regex {
		matches, _ := regexp.MatchString(re, link)
		if matches {
			return true
		}
	}
	return false
}

func matchURLRegex(links []string, regex []string) []string {
	var results []string
	for _, link := range links {
		matches := matchURLRegexLink(link, regex)
		if matches {
			results = append(results, link)
		}
	}
	return results
}

func isNon200(link string, headers map[string]string, excludedStatusCodes []int, excludedURLRegex []string) bool {
	// check if the link matches any excluded regex
	for _, regex := range excludedURLRegex {
		matches, _ := regexp.MatchString(regex, link)
		if matches {
			return false
		}
	}

	res, err := httpGET(link, headers)

	// check if the link doesn't respond properly
	if err != nil {
		return false
	}

	if res.StatusCode == 200 {
		return false
	}

	// check if the link responds with an excluded status code
	for _, code := range excludedStatusCodes {
		if res.StatusCode == code {
			return false
		}
	}
	return true
}
