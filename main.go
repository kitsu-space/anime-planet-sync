package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"sync"

	"github.com/algolia/algoliasearch-client-go/algoliasearch"
	"github.com/cheggaaa/pb"
	"github.com/go-resty/resty"
	"github.com/gocolly/colly"
	jsoniter "github.com/json-iterator/go"
	"go.uber.org/ratelimit"
	"golang.org/x/oauth2"
)

var statusMap = map[string]string{
	"status1": "completed",
	"status2": "current",
	"status3": "dropped",
	"status4": "planned",
	"status5": "on_hold",
	"status6": "dropped",
}

var index algoliasearch.Index
var f *os.File

func main() {
	flag.Parse()
	if flag.NArg() < 3 {
		log.Print("Gib anime-planet username, kitsu username, kitsu password.")
		return
	}
	var err error

	f, err = os.OpenFile("dump.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	log.SetOutput(f)

	var (
		animePlanetUsername = flag.Arg(0)
		kitsuUsername       = flag.Arg(1)
		kitsuPassword       = flag.Arg(2)
	)

	oauthConfig := oauth2.Config{
		ClientID:     "",
		ClientSecret: "",
		Endpoint: oauth2.Endpoint{
			TokenURL: "https://kitsu.io/api/oauth/token",
		},
	}

	tokens, err := oauthConfig.PasswordCredentialsToken(context.TODO(), kitsuUsername, kitsuPassword)
	if err != nil {
		panic(err)
	}

	if tokens.AccessToken == "" {
		panic("failed to obtain access token")
	}

	c := resty.New().SetAuthToken(tokens.AccessToken).SetHeaders(map[string]string{
		"Accept":       "application/vnd.api+json",
		"Content-Type": "application/vnd.api+json",
		"User-Agent":   "anime-planet-sync/1.0.0 (github.com/kitsu-space/anime-planet-sync)",
	})

	u := getUserID(c)

	baseURL := fmt.Sprintf("https://www.anime-planet.com/users/%s/%%s?sort=title&page=%%d&per_page=240&mylist_view=list", animePlanetUsername)

	algoClient := algoliasearch.NewClient("AWQO5J657S", "MTc1MWYzYzNiMjVjZjM5OWFiMDc1YWE4NTNkYWMyZjE4NTk2YjkyNjM1YWJkOWIwZTEwM2U1YmUyMjgyODIwY3Jlc3RyaWN0SW5kaWNlcz1wcm9kdWN0aW9uX21lZGlhJmZpbHRlcnM9")
	index = algoClient.InitIndex("production_media")

	kinds := [...]string{"anime", "manga"}
	pbs := make([]*pb.ProgressBar, len(kinds))
	chans := make([]<-chan Media, len(kinds))
	for idx, kind := range kinds {
		chans[idx], pbs[idx] = fetch(baseURL, kind)
	}
	_, err = pb.StartPool(pbs...)
	if err != nil {
		panic(err)
	}
	for m := range merge(chans...) {
		if err := commit(c, pkg(m, u)); err != nil {
			log.Print(err)
		}
	}
}

func pkg(m Media, uid string) Parcel {
	return Parcel{
		Data: ParcelData{
			Attributes: m.Attributes,
			Relationships: map[string]LinkWrapper{
				m.Type: {m.Link},
				"user": {Link{ID: uid, Type: "users"}},
			},
			Type: "library-entries",
		},
	}
}

type (
	Media struct {
		Attributes
		Link
	}
	Parcel struct {
		Data ParcelData `json:"data"`
	}
	Attributes struct {
		Status   string `json:"status"`
		Progress int    `json:"progress,omitempty"`
		Rating   int    `json:"ratingTwenty,omitempty"`
	}
	Link struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	LinkWrapper struct {
		Data Link `json:"data"`
	}
	ParcelData struct {
		Attributes    Attributes             `json:"attributes"`
		Relationships map[string]LinkWrapper `json:"relationships"`
		Type          string                 `json:"type"`
	}
)

func getUserID(c *resty.Client) string {
	r, _ := c.R().SetQueryParam("filter[self]", "true").Get("https://kitsu.io/api/edge/users")
	return jsoniter.Get(r.Body(), "data", 0, "id").ToString()
}

func commit(c *resty.Client, p Parcel) error {
	data, err := jsoniter.MarshalToString(p)
	if err != nil {
		return err
	}
	_, err = c.R().SetBody(data).Post("https://kitsu.io/api/edge/library-entries")
	if err != nil {
		return err
	}
	log.Println(data)
	return err
}

func fetch(baseURL, kind string) (results chan Media, p *pb.ProgressBar) {
	c := colly.NewCollector()
	rl := ratelimit.New(2)

	qk := fmt.Sprintf("kind:%s", kind)

	results = make(chan Media)
	wg := sync.WaitGroup{}
	wg.Add(1)

	c.OnHTML("p b", func(e *colly.HTMLElement) {
		if p != nil {
			return
		}
		u, err := strconv.ParseInt(e.Text, 10, 64)
		if err == nil {
			p = pb.New64(u).Prefix(kind)
		}
		wg.Done()
	})

	c.OnHTML(".personalList tbody tr", func(e *colly.HTMLElement) {
		// log.Println(e.ChildText("td.tableTitle"), e.ChildText("td.tableType"), e.ChildText("td.tableStatus span"))
		rl.Take()
		res, err := index.Search(e.ChildText("td.tableTitle"), algoliasearch.Map{
			"facetFilters":         []string{qk},
			"hitsPerPage":          1,
			"attributesToRetrieve": []string{"id"},
		})
		if err != nil {
			log.Print(err)
		} else if len(res.Hits) > 0 {
			hit := res.Hits[0]
			status, ok := statusMap[e.ChildAttr("td.tableStatus span", "class")]
			if !ok {
				status = "planned"
			}
			parsedRating, _ := strconv.ParseFloat(e.ChildAttr("td.tableRating .starrating div", "name"), 64)

			m := Media{
				Attributes: Attributes{
					Status: status,
				},
				Link: Link{
					Type: kind,
					ID:   strconv.FormatFloat(hit["id"].(float64), 'f', 0, 64),
				},
			}

			if status != "completed" {
				m.Progress, _ = strconv.Atoi(e.ChildText("td.tableEps"))
			}

			if parsedRating != 0 {
				m.Rating = int(parsedRating * 4)
			}

			results <- m
		}
		p.Increment()
	})

	re := regexp.MustCompile("[0-9]+")
	c.OnHTML(".next a", func(e *colly.HTMLElement) {
		u, err := strconv.ParseUint(re.FindAllString(e.Attr("href"), -1)[0], 10, 64)
		if err == nil {
			c.Visit(fmt.Sprintf(baseURL, kind, u))
		}
	})

	// Before making a request print "Visiting ..."
	c.OnRequest(func(r *colly.Request) {
		log.Println("Visiting", r.URL.String())
	})

	// Set error handler
	c.OnError(func(r *colly.Response, err error) {
		log.Println("Request URL:", r.Request.URL, "failed with response:", r, "\nError:", err)
	})

	go func() {
		c.Visit(fmt.Sprintf(baseURL, kind, 1))
		close(results)
	}()

	wg.Wait()
	return
}

func merge(cs ...<-chan Media) <-chan Media {
	var wg sync.WaitGroup
	out := make(chan Media)

	output := func(c <-chan Media) {
		for n := range c {
			out <- n
		}
		wg.Done()
	}
	wg.Add(len(cs))
	for _, c := range cs {
		go output(c)
	}

	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
