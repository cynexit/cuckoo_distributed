package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"time"

	"git.sec.in.tum.de/cvp/distributed-cuckoo/lib"
	"github.com/streadway/amqp"
)

type config struct {
	Amqp           string
	ConsumerQueue  string
	ProducerQueue  string
	FailedQueue    string
	VerifySSL      bool
	CheckFreeSpace bool
	CuckooURL      string
	PrefetchCount  int
	MaxPending     int
	LogFile        string
	LogLevel       string
}

var (
	c              *lib.Core
	cuckoo         *lib.CuckooConn
	producer       *lib.QueueHandler
	checkFreeSpace bool
	maxPending     = 0
)

func main() {
	var confPath string
	flag.StringVar(&confPath, "config", "", "Path to the config file")
	flag.Parse()

	if confPath == "" {
		confPath, _ = filepath.Abs(filepath.Dir(os.Args[0]))
		confPath += "/feed_cuckoo.conf.json"
	}

	conf := &config{}
	cfile, _ := os.Open(confPath)
	decoder := json.NewDecoder(cfile)
	err := decoder.Decode(&conf)
	if err != nil {
		panic("Could not decode feed_cuckoo.conf.json without errors! " + err.Error())
	}

	// setup
	c = lib.Init("feed_cuckoo", conf.Amqp, conf.LogFile, conf.LogLevel, conf.FailedQueue, conf.VerifySSL)
	checkFreeSpace = conf.CheckFreeSpace
	maxPending = conf.MaxPending
	cuckoo = c.NewCuckoo(conf.CuckooURL)
	producer = c.SetupQueue(conf.ProducerQueue)
	c.Consume(conf.ConsumerQueue, conf.PrefetchCount, parseMsg)
}

// parseMsg accepts an *amqp.Delivery and parses the body assuming
// it's a request from crits. On success the parsed struct is
// send to handleSubmit.
func parseMsg(msg amqp.Delivery) {
	m := &lib.DistributedCuckooReq{}
	err := json.Unmarshal(msg.Body, m)
	if c.NackOnError(err, "Could not decode json!", &msg) {
		return
	}

	if c.NackOnError(m.Validate(), "Error in msg from Crits service!", &msg) {
		return
	}

	fileBytes, err := base64.StdEncoding.DecodeString(m.File["data"])
	if c.NackOnError(err, "Couldn't decode msg!", &msg) {
		return
	}

	// spawn in a new thread since the request
	// via web can be time consuming and we don't
	// want to slow down the queue processing
	go handleSubmit(m, fileBytes, &msg)
}

// handleSubmit schedules the upload of a new sample to
// cuckoo. On success the information is passed to the
// check_results queue.
func handleSubmit(m *lib.DistributedCuckooReq, fileBytes []byte, msg *amqp.Delivery) {
	cStatus, _ := cuckoo.GetStatus()

	// check if cuckoo is on it's limit
	// wait if there are to much pending jobs OR the free discspace is below 256MB
	for cStatus.Tasks.Pending >= maxPending || (checkFreeSpace && cStatus.Diskspace.Analyses.Free <= 256*1024*1024) {
		c.Info.Printf("Slowdown: %d pending jobs, %d MB free space\n", cStatus.Tasks.Pending, (cStatus.Diskspace.Analyses.Free / 1024 / 1024))
		time.Sleep(time.Second * 30)
		cStatus, _ = cuckoo.GetStatus()
	}

	id, err := cuckoo.NewTask(fileBytes, m.File["name"], m.Payload)
	if c.NackOnError(err, "Uploading sample to cuckoo failed!", msg) {
		return
	}

	fcReq, err := json.Marshal(lib.FeedCuckooReq{
		id,
		cuckoo.URL,
		m.CritsData,
	})
	if c.NackOnError(err, "Could not create feedCuckooReq!", msg) {
		return
	}

	producer.Send(fcReq)
	if err := msg.Ack(false); err != nil {
		c.Warning.Println("Sending ACK failed!", err.Error())
	}
}
