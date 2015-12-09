package main

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"crypto/md5"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"git.sec.in.tum.de/cvp/distributed-cuckoo/lib"
	"github.com/streadway/amqp"
)

type config struct {
	Amqp            string
	ConsumerQueue   string
	ProducerQueue   string
	FailedQueue     string
	VerifySSL       bool
	PrefetchCount   int
	PushApiCallsMax int
	CuckooCleanup   bool
	EnabledParsers  []string
	LogFile         string
	LogLevel        string
}

type critsForgeRelReq struct {
	Action    string `json:"action"`
	RightType string `json:"right_type"`
	RightId   string `json:"right_id"`
	RelType   string `json:"rel_type"`
}

type critsServicesResp struct {
	ReturnCode int    `json:"return_code"`
	ErrorMsg   string `json:"error_message"`
	Message    string `json:"message"`
	Id         string `json:"id"`
	Type       string `json:"type"`
}

var (
	c               *lib.Core
	producer        *lib.QueueHandler
	cuckooCleanup   bool
	pushApiCallsMax int
	enabledParsers  = make(map[string]bool)
)

func main() {
	var confPath string
	flag.StringVar(&confPath, "config", "", "Path to the config file")
	flag.Parse()

	if confPath == "" {
		confPath, _ = filepath.Abs(filepath.Dir(os.Args[0]))
		confPath += "/parse_and_submit.conf.json"
	}

	conf := &config{}
	cfile, _ := os.Open(confPath)
	decoder := json.NewDecoder(cfile)
	err := decoder.Decode(&conf)
	if err != nil {
		panic("Could not decode parse_and_submit.conf.json without errors! " + err.Error())
	}

	// setup
	c = lib.Init("parse_and_submit", conf.Amqp, conf.LogFile, conf.LogLevel, conf.FailedQueue, conf.VerifySSL)
	pushApiCallsMax = conf.PushApiCallsMax
	cuckooCleanup = conf.CuckooCleanup

	if conf.ProducerQueue != "" {
		producer = c.SetupQueue(conf.ProducerQueue)
	}

	for _, e := range conf.EnabledParsers {
		enabledParsers[e] = true
	}

	c.Consume(conf.ConsumerQueue, conf.PrefetchCount, parseMsg)
}

// parseMsg accepts an *amqp.Delivery and parses the body assuming
// it's a request from check_results.
func parseMsg(msg amqp.Delivery) {
	m := &lib.CheckResultsReq{}
	err := json.Unmarshal(msg.Body, m)
	if c.NackOnError(err, "Could not decode json!", &msg) {
		return
	}

	if c.NackOnError(m.Validate(), "Error in msg from check_results service!", &msg) {
		return
	}

	// number of threads (and therby concurrent connections to crits)
	// can be indirectly controled by the PrefetchCount param in the
	// config file.
	go reportToCrits(m, &msg)
	return
}

func reportToCrits(m *lib.CheckResultsReq, msg *amqp.Delivery) {
	start := time.Now()

	// there is a chance that we get this far
	// when cuckoo is done with the job but still
	// parsing something so we do a short wait.
	time.Sleep(time.Second * 5)

	// TODO: check if an invalid machine was specified -> no results

	crits := c.NewCrits(m.CritsData)
	cuckoo := c.NewCuckoo(m.CuckooURL)

	report, err := cuckoo.TaskReport(m.TaskId)
	if c.NackOnError(err, "Couldn't load report from cuckoo!", msg) {
		return
	}

	resStructs := []*lib.CrtResult{}
	isSet := false

	// info
	if _, isSet = enabledParsers["info"]; isSet {
		resStructs = processReportInfo(report.Info)
	}

	// signatures
	if _, isSet = enabledParsers["signatures"]; isSet {
		resStructs = append(resStructs, processReportSignatures(report.Signatures)...)
	}

	// behavior
	if _, isSet = enabledParsers["behavior"]; isSet {
		resStructs = append(resStructs, processReportBehavior(report.Behavior)...)
	}

	// dropped files
	if _, isSet = enabledParsers["dropped"]; isSet {
		dResStructs, err := processDropped(m, cuckoo, crits)
		//if c.NackOnError(err, "processDropped failed", msg) {
		//	return
		//}
		if err != nil {
			c.Warning.Println("processDropped () exited with", err, "after dropping", len(dResStructs))
		}
		resStructs = append(resStructs, dResStructs...)
	}

	// parsing is done
	err = crits.AddResults(resStructs)
	if c.NackOnError(err, "Adding results to crits failed!", msg) {
		return
	}

	if cuckooCleanup {
		if err = cuckoo.DeleteTask(m.TaskId); err != nil {
			c.Warning.Println("Cleaning cuckoo up failed", err.Error())
		}
	}

	if producer != nil {
		producer.Send(msg.Body)
	}

	elapsed := time.Since(start)
	c.Info.Printf("Finished object %s [cuckoo: %d] in %s \n", m.CritsData.ObjectId, m.TaskId, elapsed)
	if err := msg.Ack(false); err != nil {
		c.Warning.Println("Sending ACK failed!", err.Error())
	}
}

// processReportInfo extracts all the data from the info
// section of the cuckoo report struct.
func processReportInfo(i *lib.CkoTasksReportInfo) []*lib.CrtResult {
	if i == nil {
		return []*lib.CrtResult{}
	}

	// i.machine can be string or struct so we
	// have to determin where to get our info
	machineString := ""
	err := json.Unmarshal(i.Machine, &machineString)
	if err != nil || machineString == "" {
		machineString = "FAILED"
	}

	mStruct := &lib.CkoTasksReportInfoMachine{}
	err = json.Unmarshal(i.Machine, mStruct)
	if err == nil && mStruct != nil {
		machineString = mStruct.Name
	}

	resMap := make(map[string]interface{})
	resMap["started"] = i.Started
	resMap["ended"] = i.Ended
	resMap["analysis_id"] = strconv.Itoa(i.Id)

	return []*lib.CrtResult{&lib.CrtResult{"info", machineString, resMap}}
}

// processReportSignatures extracts all the data from the signatures
// section of the cuckoo report struct.
func processReportSignatures(sigs []*lib.CkoTasksReportSignature) []*lib.CrtResult {
	if sigs == nil {
		return []*lib.CrtResult{}
	}

	l := len(sigs)
	res := make([]*lib.CrtResult, l, l)
	resMap := make(map[string]interface{})

	for k, sig := range sigs {
		resMap["severity"] = strconv.Itoa(sig.Severity)
		resMap["name"] = sig.Name

		res[k] = &lib.CrtResult{
			"signature",
			sig.Description,
			resMap,
		}
	}

	return res
}

// processReportBehavior extracts all the data from the behavior
// section of the cuckoo report struct.
func processReportBehavior(behavior *lib.CkoTasksReportBehavior) []*lib.CrtResult {
	if behavior == nil {
		return []*lib.CrtResult{}
	}

	var res []*lib.CrtResult
	resMap := make(map[string]interface{})

	if behavior.Processes != nil {
		for _, p := range behavior.Processes {
			resMap["process_id"] = strconv.Itoa(p.Id)
			resMap["parent_id"] = strconv.Itoa(p.ParentId)
			resMap["first_seen"] = p.FirstSeen

			res = append(res, &lib.CrtResult{
				"process",
				p.Name,
				resMap,
			})
		}
	}

	// push api calls
	// not mixed in with upper loop so we can make it optional later
	if behavior.Processes != nil {
		pushCounter := 0

		for _, p := range behavior.Processes {

			procDescription := fmt.Sprintf("%s (%d)", p.Name, p.Id)
			for _, c := range p.Calls {

				if pushCounter >= pushApiCallsMax {
					break
				}

				resMap := make(map[string]interface{})
				resMap["category"] = c.Category
				resMap["status"] = c.Status
				resMap["return"] = c.Return
				resMap["timestamp"] = c.Timestamp
				resMap["thread_id"] = c.ThreadId
				resMap["repeated"] = c.Repeated
				resMap["api"] = c.Api
				resMap["id"] = c.Id
				resMap["process"] = procDescription
				resMap["arguments"] = c.Arguments

				res = append(res, &lib.CrtResult{
					"api_call",
					c.Api,
					resMap,
				})
				pushCounter += 1
			}
		}
	}

	if behavior.Summary.Files != nil {
		for _, b := range behavior.Summary.Files {
			res = append(res, &lib.CrtResult{
				"file",
				b,
				nil,
			})
		}
	}

	if behavior.Summary.Keys != nil {
		for _, b := range behavior.Summary.Keys {
			res = append(res, &lib.CrtResult{
				"registry_key",
				b,
				nil,
			})
		}
	}

	if behavior.Summary.Mutexes != nil {
		for _, b := range behavior.Summary.Mutexes {
			res = append(res, &lib.CrtResult{
				"mutex",
				b,
				nil,
			})
		}
	}

	return res
}

func processDropped(m *lib.CheckResultsReq, cuckoo *lib.CuckooConn, crits *lib.CritsConn) ([]*lib.CrtResult, error) {
	start := time.Now()

	resp, err := cuckoo.GetDropped(m.TaskId)
	if err != nil {
		return []*lib.CrtResult{}, err
	}

	results := []*lib.CrtResult{}

	respReader := bytes.NewReader(resp)
	unbzip2 := bzip2.NewReader(respReader)
	untar := tar.NewReader(unbzip2)

	for {
		hdr, err := untar.Next()
		if err == io.EOF {
			// end of tar archive
			break
		}

		if err != nil {
			return results, err
		}

		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			// no real file, might be a dir or symlink
			continue
		}

		name := filepath.Base(hdr.Name)
		fileData, err := ioutil.ReadAll(untar)

		id, err := crits.NewSample(fileData, name)

		// we need to add a short sleep here so tastypie won't crash.
		// this is a very ugly work around but sadly necessary
		time.Sleep(time.Second * 1)

		if err != nil {
			if err.Error() == "empty file" {
				continue
			}

			return results, err
		}

		if err = crits.ForgeRelationship(id); err != nil {
			return results, err
		}

		// see comment above
		time.Sleep(time.Second * 1)

		resMap := make(map[string]interface{})
		resMap["md5"] = fmt.Sprintf("%x", md5.Sum(fileData))

		results = append(results, &lib.CrtResult{
			"file_added",
			name,
			resMap,
		})
	}

	elapsed := time.Since(start)
	c.Debug.Printf("Uploaded %d dropped files in %s [%s]\n", len(results), elapsed, m.CritsData.AnalysisId)

	return results, nil
}
