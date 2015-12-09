package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"time"

	"github.com/cynexit/cuckoo_distributed/lib"
	"github.com/streadway/amqp"
)

type config struct {
	Amqp                string
	ConsumerQueue       string
	ProducerQueue       string
	FailedQueue         string
	VerifySSL           bool
	PrefetchCount       int
	WaitBetweenRequests int
	LogFile             string
	LogLevel            string
}

type watchElem struct {
	Req *lib.FeedCuckooReq
	Msg *amqp.Delivery
}

var (
	c        *lib.Core
	producer *lib.QueueHandler
	wbr      int
	watchMap = make(map[string]*watchElem)
)

func main() {
	var confPath string
	flag.StringVar(&confPath, "config", "", "Path to the config file")
	flag.Parse()

	if confPath == "" {
		confPath, _ = filepath.Abs(filepath.Dir(os.Args[0]))
		confPath += "/check_results.conf.json"
	}

	conf := &config{}
	cfile, _ := os.Open(confPath)
	decoder := json.NewDecoder(cfile)
	err := decoder.Decode(&conf)
	if err != nil {
		panic("Could not decode check_results.conf.json without errors! " + err.Error())
	}

	// setup
	c = lib.Init("check_results", conf.Amqp, conf.LogFile, conf.LogLevel, conf.FailedQueue, conf.VerifySSL)
	wbr = conf.WaitBetweenRequests
	producer = c.SetupQueue(conf.ProducerQueue)

	go checkLoop()
	c.Consume(conf.ConsumerQueue, conf.PrefetchCount, parseMsg)
}

// parseMsg accepts an *amqp.Delivery and parses the body assuming
// it's a request from feed_cuckoo. On success the parsed struct is
// added to the watchMap.
func parseMsg(msg amqp.Delivery) {
	m := &lib.FeedCuckooReq{}
	err := json.Unmarshal(msg.Body, m)
	if c.NackOnError(err, "Could not decode json!", &msg) {
		return
	}

	if c.NackOnError(m.Validate(), "Error in msg from feed_cuckoo service!", &msg) {
		return
	}

	// add to the monitoring map
	// TODO: make sure crits analysis_id is really unique
	watchMap[m.CritsData.AnalysisId] = &watchElem{Req: m, Msg: &msg}
}

// checkLoop loops over the watch map and checks if Cuckko is done
// analysing a sample. If so it sends the task over to parse_and_submit.
func checkLoop() {
	waitDuration := time.Second * time.Duration(wbr)
	for {
		time.Sleep(waitDuration)

		for k, v := range watchMap {
			time.Sleep(waitDuration)

			cuckoo := c.NewCuckoo(v.Req.CuckooURL)
			status, err := cuckoo.TaskStatus(v.Req.TaskId)

			if c.NackOnError(err, "Couldn't get cuckoo status of task!", v.Msg) {
				delete(watchMap, k)
				continue
			}

			if status != "reported" {
				continue
			}

			crMsg, err := json.Marshal(lib.CheckResultsReq{
				v.Req.CuckooURL,
				v.Req.TaskId,
				v.Req.CritsData,
			})
			if c.NackOnError(err, "Could not create CheckResultsReq!", v.Msg) {
				delete(watchMap, k)
				continue
			}

			producer.Send(crMsg)
			if err := v.Msg.Ack(false); err != nil {
				c.Warning.Println("Sending ACK failed!", err.Error())
			}

			delete(watchMap, k)
		}
	}
}
