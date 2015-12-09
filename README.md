# A distributed cuckoo service for CRITs

A common problem with CRITs is handling multiple long running services
since CRITs is designed to keep connections to the service open until
a response is sent. This can lead to a quick saturation of available resources
which slows down the whole ecosystem significantly.

Since we hit this problem especially with cuckoo we decided to move
the whole service to a distributed and better suited model.

## Concept

The whole service consists of four microservices which communicate
using AMQP. The services are connected to cuckoo and CRITs using their
API over HTTP.

![cuckoo_distributed](https://cloud.githubusercontent.com/assets/3159191/11685641/d58e5456-9e7a-11e5-99c1-f1115075841d.png)

The cycle of a new sample looks as follows:

1. CRITs runs the service
2. The service sends an AMQP message
3. The message is received by feed_cuckoo
4. feed_cukoo schedules and performs the upload to cuckoo
5. On success feed_cuckoo sends another AMQP message to check_results
6. check_results periodically checks the status of the analysis
7. When it's done check_results sends another AMQP message to parse_and_submit
8. parse_and_submit evaluates the results and sends the results back to CRITs

As you can see there are three microservices mentioned here. The last one is not
mentioned because it's the only optional microservice. It's called overseer and
does exactly that. Whenever a microservice fails it relays the failed message to
the overseer, he'll then resubmit the failed message three times, if it fails more
than that it will dump the failed message into a text file for further analysis
what went wrong. This makes the whole system very failsafe since none of your
messages will actually get lost at any point in time.

## Setup

Setting up the services is pretty easy. The only thing you need besides a working
Cuckoo and CRITs instance is a AMQP server, we recommend RabbitMQ which is really
easy to install and setup.


### 1 Install dependencies

[RabbitMQ](https://www.rabbitmq.com/download.html)

[Go](https://golang.org/doc/install)

That's all. If you don't plan to use RabbitMQ for anything else you can safely 
use the default guest user. If this is not the case it is advised to create a
new user.


### 2 Build the microservices

You'll need a working Go compiler and environment (v1.5) to compile the code.

```
go get github.com/cynexit/cuckoo_distributed
go get github.com/streadway/amqp
```

After this you can simply get into the directory of each service and build it using

```
go build
```

Since this results in a statically linked binary you can distribute these to other
servers if you so desire.


## Configurations

Each service has it's own config file. You may pass the path to the config file to
each service via the `config` flag. If the flag is not set the service will look in
it's current directory for a file called `SERVICENAME.conf.json`.

### Common options

Lines found in most config files are

<dl>
  <dt>Amqp</dt>
  <dd>AMQP connection string in the form of `amqp://USER:PASSWORD@HOST:PORT`</dd>
  
  <dt>ConsumerQueue</dt>
  <dd>The name of the queue to consume from</dd>
  
  <dt>ProducerQueue</dt>
  <dd>The name of the queue to produce to</dd>
  
  <dt>FailedQueue</dt>
  <dd>The name of the queue to send failed messages to</dd>
  
  <dt>VerifySSL</dt>
  <dd>Check HTTPS certificates</dd>
  
  <dt>LogFile</dt>
  <dd>Full path to the log file OR empty to use only stdout</dd>
  
  <dt>LogLevel</dt>
  <dd>Choose between `debug`, `info`, and `warning`</dd>
</dl>


### feed_cuckoo.conf

<dl>
  <dt>CheckFreeSpace<dt>
  <dd>Only send new samples to Cuckoo if there is enough free space</dd>

  <dt>CuckooURL<dt>
  <dd>The url of your Cuckoo instance in the form of "https://cuckoo.your.network:PORT",</dd>

  <dt>PrefetchCount<dt>
  <dd>How many files should be handled simultaneously (Recommended: 1)</dd>

  <dt>MaxPending<dt>
  <dd>Only send new samples to Cuckoo if there are less "pending samples" than this value</dd>
</dl>

Caution: To use `CheckFreeSpace` Cuckoo needs to be of version 1.3 or higher! If you still use 1.2
(like most people do) please patch `utils/api.py` (configure `reports` and `samples`):

```python
def cuckoo_status():
    paths = dict(
        reports="/data/cuckoo/storage/analyses",
        samples="/data/cuckoo/storage/binaries",
    )

    diskspace = {}
    for key, path in paths.items():
        if hasattr(os, "statvfs"):
            stats = os.statvfs(path)
            diskspace[key] = dict(
                free=stats.f_bavail * stats.f_frsize,
                total=stats.f_blocks * stats.f_frsize,
                used=(stats.f_blocks - stats.f_bavail) * stats.f_frsize,
            )

    response = dict(
        version=CUCKOO_VERSION,
        hostname=socket.gethostname(),
        machines=dict(
            total=len(db.list_machines()),
            available=db.count_machines_available()
        ),
        diskspace=diskspace,
        tasks=dict(
            total=db.count_tasks(),
            pending=db.count_tasks("pending"),
            running=db.count_tasks("running"),
            completed=db.count_tasks("completed"),
            reported=db.count_tasks("reported")
        ),
    )

    return jsonize(response)
```


### check_results.conf

<dl>
  <dt>PrefetchCount</dt>
  <dd>How many samples should be checked for completion in the main loop? (Recommended: 100)</dd>

  <dt>WaitBetweenRequests</dt>
  <dd>Seconds to wait between each request to a Cuckoo instance? (Recommended: 5)</dd>
</dl>

### parse_and_submit.conf

<dl>
  <dt>PrefetchCount</dt>
  <dd>How many samples should be handled at the same time? (Recommended: 10)</dd>

  <dt>PushApiCallsMax</dt>
  <dd>How many of the found API calls should be send to CRITs?</dd>

  <dt>CuckooCleanup</dt>
  <dd>Delete the sample and results from Cuckoo on finish (frees space on disks)</dd>

  <dt>EnabledParsers</dt>
  <dd>Which information should be parsed? `info`, `signatures`, `behavior`, `dropped`</dd>
</dl>

`ConsumerQueue` and `ProducerQueue` are different when it comes to this service since you can
actually "chain" multiple instances of this service. This is useful if you don't want one service
to parse all the information at once but just a small and fast subset.

Example: Parsing `info`, `signatures`, and `behavior` is rather fast, parsing dropped files is rather slow
(since each dropped file needs to be uploaded to CRITs). So you probably want to have two instances of
parse_and_submit running: One which will receive the initial "sample is analyzed" message from check_results
and then starts to parse everything except dropped files and then another instance which receives another
message from the first instance and then parses only the dropped files and deletes the analysis results
from Cuckoo on success.

The two config files for this example would look as follows:

For the first instance:
```
{
	"Amqp": "amqp://guest:guest@localhost:5672",
	"ConsumerQueue": "worker/parse_and_submit",
	"ProducerQueue": "worker/parse_and_submit_dropped",
	"FailedQueue": "worker/failed",
	"VerifySSL": true,
	"PrefetchCount": 10,
	"PushApiCallsMax": 5000,
	"CuckooCleanup": false,
	"EnabledParsers": ["info", "signatures", "behavior"],
	"LogFile": "/var/log/parse_and_submit.log",
	"LogLevel": "info"
}
```

For the second instance:
```
{
	"Amqp": "amqp://guest:guest@localhost:5672",
	"ConsumerQueue": "worker/parse_and_submit_dropped",
	"ProducerQueue": "worker/parse_and_submit",
	"FailedQueue": "worker/failed",
	"VerifySSL": true,
	"PrefetchCount": 10,
	"PushApiCallsMax": 5000,
	"CuckooCleanup": true,
	"EnabledParsers": ["dropped"],
	"LogFile": "/var/log/parse_and_submit_dropped.log",
	"LogLevel": "info"
}
```

As you can see the second instance consumes from the producer queue of the second instance. If `ProducerQueue`
is empty the message will not be relayed further.


### overseer.conf

<dl>
  <dt>ConsumerQueue</dt>
  <dd>The queue you used as `FailedQueue` everywhere else</dd>

  <dt>PrefetchCount</dt>
  <dd>How many messages should be parsed simultaneously? (Recommended: 100)</dd>

  <dt>DumpDir</dt>
  <dd>The folder to dump messages into that failed three times.</dd>
</dl>


## Multiple instances

Running multiple instances of any microservice is very easy: just lunch them!
If you see that one service is taking much more time just spawn another instance
on another service and the AMQP broker will handle dividing the load between both.
This also means that if you have more than one Cuckoo instance you can simply start
another instance of feed_cuckoo and pass it a new config with a different Cuckoo URL.
This way it is really easy to scale your analysis if needed.

So a more complex scenario would look like this:

![cuckoo_distributed_complex](https://cloud.githubusercontent.com/assets/3159191/11685642/d58f782c-9e7a-11e5-8359-b9a2ea844b76.png)


## Known bottlenecks

There are two known bottlenecks, Cuckoo and CRITs.

Cuckoo is easy: Just create a new master and set up more workers and a new
feed_cuckoo instance, done.

Now with CRITs it's another story: The API is kind of slow and you need to make sure that
your mongo cluster is rock solid and your webserver is configured adequate to your expected
workload. Setting up CRITs in daemon mode can help here but just be aware that if
parse_and_submit is going slowly it's most likely a problem with CRITs. Set the log level
to `debug` to get timing information.


## Known issues 

* The samples are send over via AMQP not via CRITS API