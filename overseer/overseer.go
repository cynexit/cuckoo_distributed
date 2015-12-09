package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"git.sec.in.tum.de/cvp/distributed-cuckoo/lib"
	"github.com/streadway/amqp"
)

type config struct {
	Amqp          string
	ConsumerQueue string
	PrefetchCount int
	DumpDir       string
	LogFile       string
	LogLevel      string
}

type genericMsg struct {
	//TODO: make this shit better....
	CritsData  *lib.CritsData `json:"CritsData"`
	CritsDataJ *lib.CritsData `json:"crits_data"`
}

var (
	fsMutex   = &sync.Mutex{}
	mapMutex  = &sync.Mutex{}
	c         *lib.Core
	dumpDir   string
	producers = make(map[string]*lib.QueueHandler)
	failedMap = make(map[string]int)
)

func main() {
	var doResubmitDumped bool
	var confPath string
	flag.BoolVar(&doResubmitDumped, "resubmitDumped", false, "Resubmit all files in the dump dir to the failed queue")
	flag.StringVar(&confPath, "config", "", "Path to the config file")
	flag.Parse()

	if confPath == "" {
		confPath, _ = filepath.Abs(filepath.Dir(os.Args[0]))
		confPath += "/overseer.conf.json"
	}

	conf := &config{}
	cfile, _ := os.Open(confPath)
	decoder := json.NewDecoder(cfile)
	err := decoder.Decode(&conf)
	if err != nil {
		panic("Could not decode overseer.conf.json without errors! " + err.Error())
	}

	// setup
	c = lib.Init("overseer", conf.Amqp, conf.LogFile, conf.LogLevel, conf.ConsumerQueue, true)

	dumpDir = conf.DumpDir
	testDumpDir()

	if doResubmitDumped {
		resubmitDumped(conf.ConsumerQueue)
		return
	}

	c.Consume(conf.ConsumerQueue, conf.PrefetchCount, parseMsg)
}

func parseMsg(msg amqp.Delivery) {
	m := &lib.FailedMsg{}
	err := json.Unmarshal(msg.Body, m)
	if err != nil {
		c.Info.Println("parseMsg couldn't decode the msg body!")
		dumpMsg(&msg)
		return
	}

	go handleFailed(m, &msg)
}

func dumpMsg(msg *amqp.Delivery) {
	fsMutex.Lock()
	defer fsMutex.Unlock()

	counter := 0
	baseFileName := fmt.Sprintf("%s/%d_", dumpDir, time.Now().Unix())
	fileName := ""

	for {
		fileName = fmt.Sprintf("%s%d", baseFileName, counter)
		if _, err := os.Stat(fileName); os.IsNotExist(err) {
			break
		}
		counter += 1
	}

	fp, err := os.OpenFile(fileName, os.O_RDWR|os.O_CREATE, os.ModePerm)
	c.FailOnError(err, "Couldn't create new file!")
	defer fp.Close()

	_, err = fp.Write(msg.Body)
	c.FailOnError(err, "Couldn't write to the file!")
	msg.Ack(false)
}

func testDumpDir() {
	fileName := dumpDir + "/__test"
	fileContent := []byte("testing is so much fun!")

	_ = os.Remove(fileName)
	fp, err := os.OpenFile(fileName, os.O_RDWR|os.O_CREATE, os.ModePerm)
	c.FailOnError(err, "Testing the dump dir failed! Couldn't create new file!")

	_, err = fp.Write(fileContent)
	c.FailOnError(err, "Testing the dump dir failed! Couldn't write to the file!")
	fp.Close()

	fp, err = os.OpenFile(fileName, os.O_RDWR|os.O_CREATE, os.ModePerm)
	c.FailOnError(err, "Testing the dump dir failed! Couldn't write to the file!")
	defer fp.Close()

	reader := bufio.NewReader(fp)
	contents, _ := ioutil.ReadAll(reader)

	if string(contents) != string(fileContent) {
		c.FailOnError(errors.New("Testing the dump dir failed! Couldn't read from the file!"), "")
	}

	_ = os.Remove(fileName)
}

func resubmitDumped(queue string) {
	failed := c.SetupQueue(queue)

	files, _ := ioutil.ReadDir(dumpDir)
	for _, f := range files {
		if f.IsDir() {
			continue
		}

		fname := dumpDir + "/" + f.Name()

		fp, err := os.OpenFile(fname, os.O_RDWR|os.O_CREATE, os.ModePerm)
		c.FailOnError(err, "Couldn't open file!")

		reader := bufio.NewReader(fp)
		contents, err := ioutil.ReadAll(reader)
		c.FailOnError(err, "Couldn't read file!")
		fp.Close()

		failed.Send(contents)

		_ = os.Remove(fname)
	}

}

func handleFailed(failed *lib.FailedMsg, msg *amqp.Delivery) {
	payload := &genericMsg{}
	err := json.Unmarshal([]byte(failed.Msg), payload)
	if err != nil {
		c.Info.Println("Couldn't decode genericMsg!")
		dumpMsg(msg)
		return
	}

	aid := ""
	if payload.CritsData != nil {
		aid = payload.CritsData.AnalysisId
	} else if payload.CritsDataJ != nil {
		aid = payload.CritsDataJ.AnalysisId
	} else {
		c.Info.Println("Couldn't find CritsData!")
		dumpMsg(msg)
		return
	}

	_, set := failedMap[aid]
	if !set {
		failedMap[aid] = 0
	}

	failedMap[aid] += 1

	// resubmit three times or dump
	if failedMap[aid] > 3 {
		c.Info.Println("Msg failed three times, dumping!")
		dumpMsg(msg)
		return
	}

	time.Sleep(time.Second * 60)
	err = resubmit(failed, msg)
	if err != nil {
		c.Info.Println("Resubmiting failed!", err)
		dumpMsg(msg)
		return
	}

	msg.Ack(false)
}

func resubmit(failed *lib.FailedMsg, msg *amqp.Delivery) error {
	s, err := strconv.Unquote(failed.Msg)
	if err != nil {
		s = failed.Msg
	}

	mapMutex.Lock()
	if _, exists := producers[failed.Queue]; !exists {
		producers[failed.Queue] = c.SetupQueue(failed.Queue)
	}
	mapMutex.Unlock()

	fmt.Println(s)

	producers[failed.Queue].Send([]byte(s))
	return nil
}
