package main

import (
	"bufio"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/MilesOkeefe/esbulk"
	"github.com/sethgrid/pester"
)

// Version of application.
const Version = "0.5.2"

var (
	version     = flag.Bool("v", false, "prints current program version")
	cpuprofile  = flag.String("cpuprofile", "", "write cpu profile to file")
	memprofile  = flag.String("memprofile", "", "write heap profile to file")
	indexName   = flag.String("index", "", "index name")
	docType     = flag.String("type", "default", "elasticsearch doc type")
	batchSize   = flag.Int("size", 1000, "bulk batch size")
	numWorkers  = flag.Int("w", runtime.NumCPU(), "number of workers to use")
	verbose     = flag.Bool("verbose", false, "output basic progress")
	gzipped     = flag.Bool("z", false, "unzip gz'd file on the fly")
	mapping     = flag.String("mapping", "", "mapping string or filename to apply before indexing")
	purge       = flag.Bool("purge", false, "purge any existing index before indexing")
	idfield     = flag.String("id", "", "name of field to use as id field, by default ids are autogenerated")
	user        = flag.String("u", "", "http basic auth username:password, like curl -u")
	zeroReplica = flag.Bool("0", false, "set the number of replicas to 0 during indexing")
	pipeline    = flag.String("pipeline", "", "pipeline to run")
)

// indexSettingsRequest runs updates an index setting, given a body and
// options. Body consist of the JSON document, e.g. `{"index":
// {"refresh_interval": "1s"}}`.
func indexSettingsRequest(body string, options esbulk.Options) (*http.Response, error) {
	r := strings.NewReader(body)

	rand.Seed(time.Now().Unix())
	server := options.Servers[rand.Intn(len(options.Servers))]
	link := fmt.Sprintf("%s/%s/_settings", server, options.Index)

	req, err := http.NewRequest("PUT", link, r)
	if err != nil {
		return nil, err
	}
	// Auth handling.
	if options.Username != "" && options.Password != "" {
		req.SetBasicAuth(options.Username, options.Password)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := pester.Do(req)
	if err != nil {
		return nil, err
	}
	if options.Verbose {
		log.Printf("applied setting: %s with status %s\n", body, resp.Status)
	}
	return resp, nil
}

func main() {
	var serverFlags esbulk.ArrayFlags
	flag.Var(&serverFlags, "server", "elasticsearch server, this works with https as well")

	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if *version {
		fmt.Println(Version)
		os.Exit(0)
	}

	if *indexName == "" {
		log.Fatal("index name required")
	}

	if len(serverFlags) == 0 {
		serverFlags = append(serverFlags, "http://localhost:9200")
	}

	if *verbose {
		log.Printf("using %d servers", len(serverFlags))
	}

	var file io.Reader = os.Stdin

	if flag.NArg() > 0 {
		f, err := os.Open(flag.Arg(0))
		if err != nil {
			log.Fatalln(err)
		}
		defer f.Close()
		file = f
	}

	runtime.GOMAXPROCS(*numWorkers)

	var username, password string
	if len(*user) > 0 {
		parts := strings.Split(*user, ":")
		if len(parts) != 2 {
			log.Fatal("http basic auth syntax is: username:password")
		}
		username = parts[0]
		password = parts[1]
	}

	options := esbulk.Options{
		Servers:   serverFlags,
		Index:     *indexName,
		DocType:   *docType,
		BatchSize: *batchSize,
		Verbose:   *verbose,
		Scheme:    "http",
		IDField:   *idfield,
		Username:  username,
		Password:  password,
		Pipeline:  *pipeline,
	}

	if *verbose {
		log.Println(options)
	}

	if *purge {
		if err := esbulk.DeleteIndex(options); err != nil {
			log.Fatal(err)
		}
		time.Sleep(5 * time.Second)
	}

	// create index if not exists
	//if err := esbulk.CreateIndex(options); err != nil {
	//	log.Fatal(err)
	//}
	//
	//if *mapping != "" {
		// var reader io.Reader
		// if _, err := os.Stat(*mapping); os.IsNotExist(err) {
		//   reader = strings.NewReader(*mapping)
		// } else {
		//   file, err := os.Open(*mapping)
		//   if err != nil {
		//     log.Fatal(err)
		//   }
		//   reader = bufio.NewReader(file)
		// }
		// err := esbulk.PutMapping(options, reader)
		// if err != nil {
		//   log.Fatal(err)
		// }
	// }

	queue := make(chan string)
	var wg sync.WaitGroup

	for i := 0; i < *numWorkers; i++ {
		wg.Add(1)
		go esbulk.Worker(fmt.Sprintf("worker-%d", i), options, queue, &wg)
	}

	for i, _ := range options.Servers {
		// Store number_of_replicas settings for restoration later.
		doc, err := esbulk.GetSettings(i, options)
		if err != nil {
			log.Fatal(err)
		}

		// TODO(miku): Rework this.
		numberOfReplicas := doc[options.Index].(map[string]interface{})["settings"].(map[string]interface{})["index"].(map[string]interface{})["number_of_replicas"]
		if *verbose {
			log.Printf("on shutdown, number_of_replicas will be set back to %s", numberOfReplicas)
		}

		// Shutdown procedure. TODO(miku): Handle signals, too.
		defer func() {
			// Realtime search.
			if _, err := indexSettingsRequest(`{"index": {"refresh_interval": "1s"}}`, options); err != nil {
				log.Fatal(err)
			}
			// Reset number of replicas.
			if _, err := indexSettingsRequest(fmt.Sprintf(`{"index": {"number_of_replicas": %q}}`, numberOfReplicas), options); err != nil {
				log.Fatal(err)
			}

			// Persist documents.
			if err := esbulk.FlushIndex(i, options); err != nil {
				log.Fatal(err)
			}
		}()

		// Realtime search.
		resp, err := indexSettingsRequest(`{"index": {"refresh_interval": "-1"}}`, options)
		if err != nil {
			log.Fatal(err)
		}
		if resp.StatusCode >= 400 {
			log.Fatal(resp)
		}
		if *zeroReplica {
			// Reset number of replicas.
			if _, err := indexSettingsRequest(`{"index": {"number_of_replicas": 0}}`, options); err != nil {
				log.Fatal(err)
			}
		}
	}

	reader := bufio.NewReader(file)
	if *gzipped {
		zreader, err := gzip.NewReader(file)
		if err != nil {
			log.Fatal(err)
		}
		reader = bufio.NewReader(zreader)
	}

	counter := 0
	start := time.Now()

	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		line = strings.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		queue <- line
		counter++
	}

	close(queue)
	wg.Wait()
	elapsed := time.Since(start)

	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.WriteHeapProfile(f)
		f.Close()
	}

	if *verbose {
		rate := float64(counter) / elapsed.Seconds()
		log.Printf("%d docs in %s at %0.3f docs/s with %d workers\n", counter, elapsed, rate, *numWorkers)
	}
}
