package lib

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"

	"github.com/streadway/amqp"
)

// Core struct contains all vital information for the microservices
// to run. Connection to amqp server, global HTTP client, loggers,
// and the queue for failed messages.
type Core struct {
	AmqpConn    *amqp.Connection
	Debug       *log.Logger
	Info        *log.Logger
	Warning     *log.Logger
	Client      *http.Client
	ServiceName string

	failed *QueueHandler
}

type FailedMsg struct {
	Service string
	Queue   string
	Error   string
	Desc    string
	Msg     string
}

type QueueHandler struct {
	Queue   string
	Channel *amqp.Channel
	C       *Core
}

// DistributedCuckooReq is the amqp msg sent from crits to feed_cuckoo
type DistributedCuckooReq struct {
	Payload   map[string]string `json:"payload"`
	File      map[string]string `json:"file"`
	CritsData *CritsData        `json:"crits_data"`
}

// FeedCuckooReq is the amqp msg sent from feed_cuckoo to check_results
type FeedCuckooReq struct {
	TaskId    int
	CuckooURL string
	CritsData *CritsData
}

// CheckResultsReq is the amqp msg sent from check_results to parse_and_submit
type CheckResultsReq struct {
	CuckooURL string
	TaskId    int
	CritsData *CritsData
}

// critsData contains the most important data about a analysis handled
// by crits. This data is needed to conntect to crits and is present
// in every amqp message.
type CritsData struct {
	CritsURL   string `json:"crits_url"`
	AnalysisId string `json:"analysis_id"`
	ObjectType string `json:"object_type"`
	ObjectId   string `json:"object_id"`
	Username   string `json:"username"`
	ApiKey     string `json:"api_key"`
	MD5        string `json:"md5"`
	Source     string `json:"source"`
}

// Init creates a new Core struct containing all the necessary information.
// The function also initializes loggin, the amqp connection, the failed
// queue, and HTTP client.
func Init(service, amqpConnectionPath, logPath, logLevel, failedQueue string, verifySSL bool) *Core {
	var err error
	c := &Core{}

	c.setupLogging(logPath, logLevel)
	c.ServiceName = service

	c.Info.Println("Connecting to amqp server...")
	c.AmqpConn, err = amqp.Dial(amqpConnectionPath)
	c.FailOnError(err, "Failed to connect to the amqp server!")

	c.failed = c.SetupQueue(failedQueue)

	c.setupClient(verifySSL)

	return c
}

// setupLogging populates the info and warning logger.
func (c *Core) setupLogging(file, level string) {
	// default: only log to stdout
	handler := io.MultiWriter(os.Stdout)

	if file != "" {
		// log to file
		if _, err := os.Stat(file); os.IsNotExist(err) {
			err := ioutil.WriteFile(file, []byte(""), 0600)
			c.FailOnError(err, "Couldn't create the log!")
		}

		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		c.FailOnError(err, "Failed to open log file!")

		handler = io.MultiWriter(f, os.Stdout)
	}

	// TODO: make this nicer....
	empty := io.MultiWriter()
	if level == "warning" {
		c.Warning = log.New(handler, "WARNING: ", log.Ldate|log.Ltime|log.Lshortfile)
		c.Info = log.New(empty, "INFO: ", log.Ldate|log.Ltime)
		c.Debug = log.New(empty, "DEBUG: ", log.Ldate|log.Ltime|log.Lshortfile)
	} else if level == "info" {
		c.Warning = log.New(handler, "WARNING: ", log.Ldate|log.Ltime|log.Lshortfile)
		c.Info = log.New(handler, "INFO: ", log.Ldate|log.Ltime)
		c.Debug = log.New(empty, "DEBUG: ", log.Ldate|log.Ltime|log.Lshortfile)
	} else {
		c.Warning = log.New(handler, "WARNING: ", log.Ldate|log.Ltime|log.Lshortfile)
		c.Info = log.New(handler, "INFO: ", log.Ldate|log.Ltime)
		c.Debug = log.New(handler, "DEBUG: ", log.Ldate|log.Ltime|log.Lshortfile)
	}
}

// setupClient populates the http client so we have one client
// which can keep the connections open so there is no need to
// start a new connection for each request.
func (c *Core) setupClient(verifySSL bool) {
	tr := &http.Transport{}
	if !verifySSL {
		tr = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	c.Client = &http.Client{Transport: tr}
}

// SetupQueue creates a new channel on top of the established
// amqp connection and declares a persistent queue with the
// given name. It then returns a pointer to a QueueHandler.
func (c *Core) SetupQueue(queue string) *QueueHandler {
	if queue == "" {
		c.Warning.Println("Queue name is empty! A persistent and anonymous queue will be created.")
	}

	c.Debug.Println("Creating new queue handler for", queue)

	channel, err := c.AmqpConn.Channel()
	c.FailOnError(err, "Failed to open channel")

	_, err = channel.QueueDeclare(
		queue, // name
		true,  // durable
		false, // delete when unused
		false, // exclusive
		false, // no-wait
		nil,   // arguments
	)
	c.FailOnError(err, "Failed to declare queue")

	return &QueueHandler{queue, channel, c}
}

// Send is used to send a message to a amqp
// queue. Channel and queue name are taken from
// the QueueHandler struct.
func (q *QueueHandler) Send(msg []byte) {
	err := q.Channel.Publish(
		"",      // exchange
		q.Queue, // routing key
		false,   // mandatory
		false,   // immediate
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "text/plain",
			Body:         msg,
		})
	q.C.FailOnError(err, "Failed to publish a message")

	i := ""
	if len(msg) > 700 {
		i = string(msg[:700]) + " [...]"
	} else {
		i = string(msg)
	}

	q.C.Info.Println("Dispatched", i)
}

// Consume connects to a queue as a consumer, sets the QoS
// and relays all incoming messages to the supplied function.
func (c *Core) Consume(queue string, prefetchCount int, fn func(msg amqp.Delivery)) {
	c.Debug.Println("Starting to consume on", queue)

	handle := c.SetupQueue(queue)

	err := handle.Channel.Qos(
		prefetchCount, // prefetch count
		0,             // prefetch size
		false,         // global
	)
	c.FailOnError(err, "Failed to set consumerQoS")

	msgs, err := handle.Channel.Consume(
		handle.Queue, // queue
		"",           // consumer
		false,        // auto-ack
		false,        // exclusive
		false,        // no-local
		false,        // no-wait
		nil,          // args
	)
	c.FailOnError(err, "Failed to register a consumer")

	forever := make(chan bool)

	go func() {
		for m := range msgs {
			c.Info.Println("Received a message")
			fn(m)
		}
	}()

	c.Info.Println("Connection to amqp server successful! Waiting...")
	<-forever
}

// FastGet is a wrapper for http.Get which returns only
// the important data from the request.
func (c *Core) FastGet(url string, structPointer interface{}) ([]byte, int, error) {
	c.Debug.Println("Getting", url)

	resp, err := c.Client.Get(url)
	if err != nil {
		return nil, 0, err
	}
	defer SafeResponseClose(resp)

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}

	if structPointer != nil {
		err = json.Unmarshal(respBody, structPointer)
	}

	c.Debug.Println("Done getting", url)
	return respBody, resp.StatusCode, err
}

// FastGet is a wrapper for http.PostForm which returns only
// the important data from the request.
func (c *Core) FastPostForm(url string, data url.Values, structPointer interface{}) ([]byte, int, error) {
	c.Debug.Println("Posting from to", url)
	resp, err := c.Client.PostForm(url, data)
	if err != nil {
		return nil, 0, err
	}
	defer SafeResponseClose(resp)

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}

	if structPointer != nil {
		err = json.Unmarshal(respBody, structPointer)
	}

	c.Debug.Println("Done posting form to", url)
	return respBody, resp.StatusCode, err
}

func (r *DistributedCuckooReq) Validate() error {
	// validate the struct(s)
	if r.File == nil {
		return errors.New("File map doesn't exist!")
	}

	fN, bfN := r.File["name"]
	fD, bfD := r.File["data"]
	if !bfN || !bfD || fN == "" || fD == "" {
		return errors.New("file map layout invalid!")
	}

	// since there is no way to check if the supplied crits
	// data is valid we will only check if the data is present
	if r.CritsData == nil {
		return errors.New("crits struct doesn't exist!")
	}

	return nil
}

func SafeResponseClose(r *http.Response) {
	if r == nil {
		return
	}

	io.Copy(ioutil.Discard, r.Body)
	r.Body.Close()
}

func (r *FeedCuckooReq) Validate() error {
	if r.CuckooURL == "" || r.TaskId == 0 {
		return errors.New("No CuckooURL / TaskId!")
	}

	// since there is no way to check if the supplied crits
	// data is valid we will only check if the data is present
	if r.CritsData == nil {
		return errors.New("crits struct doesn't exist!")
	}

	return nil
}

func (r *CheckResultsReq) Validate() error {
	if r.CuckooURL == "" || r.TaskId == 0 {
		return errors.New("No CuckooURL / TaskId!")
	}

	// since there is no way to check if the supplied crits
	// data is valid we will only check if the data is present
	if r.CritsData == nil {
		return errors.New("crits struct doesn't exist!")
	}

	return nil
}

func (r *FailedMsg) Validate() error {
	if r.Service == "" || r.Queue == "" {
		return errors.New("Queue or Service unknown!")
	}

	//TODO: Check msg (unmarshal)

	return nil
}

// FailOnError accepts an error and message. If the error
// is not nil the programm will panic with said message.
func (c *Core) FailOnError(err error, msg string) {
	if err != nil {
		c.Warning.Printf("%s: %s\n", msg, err)
		panic(fmt.Sprintf("%s: %s\n", msg, err))
	}
}

// nackOnError accepts an error, error description, and amqp
// message. If the error is not nil a NACK is send in replay
// to the msg. The msg will be redirected to the failed queue
// so the overseer can handle it.
func (c *Core) NackOnError(err error, desc string, msg *amqp.Delivery) bool {
	if err != nil {
		c.Warning.Println("[NACK]", desc, err.Error())

		jm, err := json.Marshal(FailedMsg{
			c.ServiceName,
			msg.RoutingKey,
			err.Error(),
			desc,
			string(msg.Body),
		})
		if err != nil {
			c.Warning.Println(err.Error())
		}

		c.failed.Send(jm)

		err = msg.Nack(false, false)
		if err != nil {
			c.Warning.Println("Sending NACK failed!", err.Error())
		}

		return true
	}

	return false
}
