package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/coreos/etcd/client"
	"github.com/dustin/go-broadcast"
	"github.com/icook/btcd/rpcclient"
	"github.com/icook/ngpool/pkg/service"
	log "github.com/inconshreveable/log15"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/mitchellh/mapstructure"
	"github.com/r3labs/sse"
	"github.com/seehuhn/sha256d"
	"github.com/spf13/viper"
	_ "github.com/spf13/viper/remote"
	"math/big"
	"net"
	"net/http"
	"sync"
	"time"
)

type BlockSolve struct {
	powhash    *big.Int
	difficulty *big.Int
	height     int64
	subsidy    int64
	data       []byte
}

func (b *BlockSolve) getBlockHash() *big.Int {
	var hasher = sha256d.New()
	hasher.Write(b.data)
	ret := big.Int{}
	ret.SetBytes(hasher.Sum(nil))
	return &ret
}

type Share struct {
	username   string
	time       time.Time
	difficulty float64
	blocks     map[string]*BlockSolve
}

type Template struct {
	key  TemplateKey
	data []byte
}

type TemplateKey struct {
	Algo         string
	Currency     string
	TemplateType string
}

type StratumServer struct {
	config   *viper.Viper
	etcd     client.Client
	etcdKeys client.KeysAPI
	db       *sqlx.DB

	coinserverWatchers map[string]*CoinserverWatcher
	newShare           chan *Share
	newTemplate        chan *Template
	jobSubscribe       chan chan interface{}
	jobCast            broadcast.Broadcaster
	service            *service.Service

	lastJob    *Job
	lastJobMtx *sync.Mutex

	// Keyed by currency code
	blockCast    map[string]broadcast.Broadcaster
	blockCastMtx *sync.Mutex
}

func NewStratumServer() *StratumServer {
	config := viper.New()

	config.SetDefault("LogLevel", "info")
	config.SetDefault("EnableCpuminer", false)
	config.SetDefault("StratumBind", "127.0.0.1:3333")
	// Load from Env so we can access etcd
	config.AutomaticEnv()

	ng := &StratumServer{
		config:             config,
		coinserverWatchers: make(map[string]*CoinserverWatcher),

		newTemplate:  make(chan *Template),
		newShare:     make(chan *Share),
		jobSubscribe: make(chan chan interface{}),
		blockCast:    make(map[string]broadcast.Broadcaster),
		blockCastMtx: &sync.Mutex{},
		lastJobMtx:   &sync.Mutex{},
		jobCast:      broadcast.NewBroadcaster(10),
	}
	ng.service = service.NewService("stratum", config)
	ng.service.SetLabels(map[string]interface{}{
		"endpoint": config.GetString("StratumBind"),
	})

	return ng
}

func (n *StratumServer) Start() {
	db, err := sqlx.Connect("postgres", n.config.GetString("DbConnectionString"))
	if err != nil {
		log.Crit("Failed to connect to db", "err", err)
		panic(err)
	}
	n.db = db

	levelConfig := n.config.GetString("LogLevel")
	level, err := log.LvlFromString(levelConfig)
	if err != nil {
		log.Crit("Unable to parse log level", "configval", levelConfig, "err", err)
	}
	handler := log.CallerFileHandler(log.StdoutHandler)
	handler = log.LvlFilterHandler(level, handler)
	log.Root().SetHandler(handler)
	log.Info("Set log level", "level", level)

	var tmplKeys []TemplateKey
	val := n.config.Get("AuxCurrencies")
	err = mapstructure.Decode(val, &tmplKeys)
	if err != nil {
		log.Error("Invalid configuration, 'AuxCurrency' of improper format", "err", err)
		return
	}

	var tmplKey TemplateKey
	val = n.config.Get("BaseCurrency")
	err = mapstructure.Decode(val, &tmplKey)
	if err != nil {
		log.Error("Invalid configuration, 'BaseCurrency' of improper format", "err", err)
		return
	}
	tmplKeys = append(tmplKeys, tmplKey)

	go n.listenTemplates()

	updates, err := n.service.ServiceWatcher("coinserver")
	if err != nil {
		log.Crit("Failed to start coinserver watcher", "err", err)
	}
	go n.HandleCoinserverWatcherUpdates(updates, tmplKeys)
	go n.service.KeepAlive()

	if n.config.GetBool("EnableCpuminer") {
		go n.Miner()
	}
	go n.ListenMiners()
	go n.ListenSubscribers()
	go n.ListenShares()
}

func (n *StratumServer) ListenShares() {
	log.Debug("Starting ListenShares")
	chainName := n.config.GetString("ShareChainName")
	for {
		share := <-n.newShare
		log.Debug("Got share", "share", share)
		for currencyCode, block := range share.blocks {
			n.blockCast[currencyCode].Submit(block)
			_, err := n.db.Exec(
				`INSERT INTO block
				(height, currency, hash, powhash, subsidy, mined_at, mined_by, difficulty, chain)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				block.height,
				currencyCode,
				hex.EncodeToString(block.getBlockHash().Bytes()),
				hex.EncodeToString(block.powhash.Bytes()),
				block.subsidy,
				share.time,
				share.username,
				block.difficulty.String(),
				chainName)
			if err != nil {
				log.Error("Failed to save block", "err", err)
			}
		}
		_, err := n.db.Exec(
			`INSERT INTO share (username, difficulty, mined_at, chain)
			VALUES ($1, $2, $3, $4)`,
			share.username, share.difficulty, share.time, chainName)
		if err != nil {
			log.Error("Failed to save share", "err", err)
		}
	}
}

func (n *StratumServer) ListenSubscribers() {
	log.Debug("Starting ListenSubscribers")
	for {
		listener := <-n.jobSubscribe
		n.lastJobMtx.Lock()
		if n.lastJob != nil {
			listener <- n.lastJob
		}
		n.lastJobMtx.Unlock()
		n.jobCast.Register(listener)
	}
}

func (n *StratumServer) listenTemplates() {
	// Starts a goroutine to listen for new templates from newTemplate channel.
	// When new templates are available a new job is created and broadcasted
	// over jobBroadcast
	latestTemp := map[TemplateKey][]byte{}
	for {
		newTemplate := <-n.newTemplate
		log.Info("Got new template", "key", newTemplate.key)
		latestTemp[newTemplate.key] = newTemplate.data
		job, err := NewJobFromTemplates(latestTemp)
		if err != nil {
			log.Error("Error generating job", "err", err)
			continue
		}
		log.Info("New job generated, pushing...")
		n.lastJobMtx.Lock()
		n.lastJob = job
		n.lastJobMtx.Unlock()
		n.jobCast.Submit(job)
	}
}

func (n *StratumServer) Miner() {
	listener := make(chan interface{})
	n.jobCast.Register(listener)
	jobLock := sync.Mutex{}
	var job *Job

	// Watch for new jobs for us
	go func() {
		for {
			jobOrig := <-listener
			newJob, ok := jobOrig.(*Job)
			if newJob == nil || !ok {
				log.Warn("Bad job from broadcast", "job", jobOrig)
				continue
			}
			jobLock.Lock()
			job = newJob
			jobLock.Unlock()
		}
	}()
	go func() {
		var i uint32 = 0
		last := time.Now()
		lasti := i
		for {
			if i%10000 == 0 {
				t := time.Now()
				dur := t.Sub(last)
				if dur > (time.Second * 15) {
					hashrate := fmt.Sprintf("%.f hps", float64(i-lasti)/dur.Seconds())
					log.Info("Hashrate", "rate", hashrate)
					lasti = i
					last = t
				}
			}
			if job == nil {
				time.Sleep(time.Second * 1)
				continue
			}
			jobLock.Lock()
			var nonce = make([]byte, 4)
			binary.BigEndian.PutUint32(nonce, i)

			solves, _, err := job.CheckSolves(nonce, extraNonceMagic, nil)
			if err != nil {
				log.Warn("Failed to check solves for job", "err", err)
			}
			for currencyCode, block := range solves {
				n.blockCast[currencyCode].Submit(block)
			}
			if len(solves) > 0 {
				time.Sleep(time.Second * 10)
			}
			jobLock.Unlock()
			i += 1
		}
	}()
}

func (n *StratumServer) Stop() {
}

type CoinserverWatcher struct {
	id          string
	tmplKey     TemplateKey
	endpoint    string
	status      string
	newTemplate chan *Template
	blockCast   broadcast.Broadcaster
	wg          sync.WaitGroup
	shutdown    chan interface{}
}

func (cw *CoinserverWatcher) Stop() {
	// Trigger the stopping of the watcher, and wait for complete shutdown (it
	// will close channel 'done' on exit)
	if cw.shutdown == nil {
		return
	}
	close(cw.shutdown)
	cw.wg.Wait()
}

func (cw *CoinserverWatcher) Start() {
	cw.wg = sync.WaitGroup{}
	cw.shutdown = make(chan interface{})
	go cw.RunTemplateBroadcaster()
	go cw.RunBlockCastListener()
}

func (cw *CoinserverWatcher) RunBlockCastListener() {
	cw.wg.Add(1)
	defer cw.wg.Done()
	logger := log.New("coin", cw.tmplKey.Currency, "id", cw.id[:8])

	connCfg := &rpcclient.ConnConfig{
		Host:         cw.endpoint[7:] + "rpc",
		User:         "",
		Pass:         "",
		HTTPPostMode: true, // Bitcoin core only supports HTTP POST mode
		DisableTLS:   true, // Bitcoin core does not provide TLS by default
	}
	client, err := rpcclient.New(connCfg, nil)
	if err != nil {
		panic(err)
	}

	listener := make(chan interface{})
	cw.blockCast.Register(listener)
	defer func() {
		logger.Debug("Closing template listener channel")
		cw.blockCast.Unregister(listener)
		close(listener)
	}()
	for {
		msg := <-listener
		newBlock := msg.(*BlockSolve)
		if err != nil {
			logger.Error("Invalid type recieved from blockCast", "err", err)
			continue
		}
		hexString := hex.EncodeToString(newBlock.data)
		encodedBlock, err := json.Marshal(hexString)
		if err != nil {
			logger.Error("Failed to json marshal a string", "err", err)
			continue
		}
		params := []json.RawMessage{
			encodedBlock,
			[]byte{'[', ']'},
		}
		rawResult, err := client.RawRequest("submitblock", params)
		res := string(rawResult[1 : len(rawResult)-1])
		if err != nil {
			logger.Info("Error submitting block", "err", err)
		} else if res == "" {
			logger.Info("Found a block!")
		} else if res == "inconclusive" {
			logger.Info("Found a block! (inconclusive)")
		} else {
			logger.Info("Maybe found a block", "resp", res)
		}
	}
}

func (cw *CoinserverWatcher) RunTemplateBroadcaster() {
	cw.wg.Add(1)
	defer cw.wg.Done()
	logger := log.New("id", cw.id[:8], "tmplKey", cw.tmplKey)
	client := &sse.Client{
		URL:        cw.endpoint + "blocks",
		Connection: &http.Client{},
		Headers:    make(map[string]string),
	}

	for {
		events := make(chan *sse.Event)
		err := client.SubscribeChan("messages", events)
		if err != nil {
			if cw.status != "down" {
				logger.Warn("CoinserverWatcher is now DOWN", "err", err)
			}
			cw.status = "down"
			select {
			case <-cw.shutdown:
				return
			case <-time.After(time.Second * 2):
			}
			continue
		}
		lastEvent := sse.Event{}
		cw.status = "up"
		logger.Debug("CoinserverWatcher is now UP")
		for {
			// Wait for new event or exit signal
			select {
			case <-cw.shutdown:
				return
			case msg := <-events:
				// When the connection breaks we get a nill pointer. Break out
				// of loop and try to reconnect
				if msg == nil {
					break
				}
				// Unfortunately this SSE library produces an event for every
				// line, instead of an event for every \n\n as is the standard.
				// So we manually combine the events into one
				if msg.Event != nil {
					lastEvent.Event = msg.Event
				}
				if msg.Data != nil {
					decoded, err := base64.StdEncoding.DecodeString(string(msg.Data))
					if err != nil {
						logger.Error("Bad payload from coinserver", "payload", decoded)
					}
					lastEvent.Data = decoded
					logger.Debug("Got new template", "data", string(decoded))
					cw.newTemplate <- &Template{
						data: lastEvent.Data,
						key:  cw.tmplKey,
					}
					if cw.status != "live" {
						logger.Info("CoinserverWatcher is now LIVE")
					}
					cw.status = "live"
				}
			}
		}
	}
}

func (n *StratumServer) ListenMiners() {
	endpoint := n.config.GetString("StratumBind")
	listener, err := net.Listen("tcp", endpoint)
	if err != nil {
		log.Crit("Failed to listen stratum", "err", err)
	}
	log.Info("Listening stratum", "endpoint", endpoint)
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Warn("Failed to accept connection", "err", err)
			continue
		}
		c := NewClient(conn, n.jobSubscribe, n.newShare)
		c.Start()
	}
}

func (n *StratumServer) HandleCoinserverWatcherUpdates(
	updates chan service.ServiceStatusUpdate, tmplKeys []TemplateKey) {
	log.Info("Listening for new coinserver services")
	for {
		update := <-updates
		switch update.Action {
		case "removed":
			if csw, ok := n.coinserverWatchers[update.ServiceID]; ok {
				log.Info("Coinserver shutdown", "id", update.ServiceID[:8])
				csw.Stop()
			}
		case "updated":
			log.Debug("Coinserver status update", "id", update.ServiceID, "new_status", update.Status)
		case "added":
			labels := update.Status.Labels
			// TODO: Should probably serialize to datatype...
			tmplKey := TemplateKey{
				Currency:     labels["currency"].(string),
				Algo:         labels["algo"].(string),
				TemplateType: labels["template_type"].(string),
			}
			// I'm sure there's a less verbose way to do this, but if we're not
			// interested in the templates of this coinserver, ignore the
			// update and continue
			found := false
			for _, key := range tmplKeys {
				if key == tmplKey {
					found = true
					break
				}
			}
			if !found {
				log.Debug("Ignoring coinserver", "id", update.ServiceID, "key", tmplKey)
				continue
			}

			// Create a watcher service that listens for block submission on
			// blockCast and pushes new templates to newTemplate channel
			blockCast := n.getBlockCast(labels["currency"].(string))
			cw := &CoinserverWatcher{
				endpoint:    labels["endpoint"].(string),
				status:      "starting",
				newTemplate: n.newTemplate,
				blockCast:   blockCast,
				id:          update.ServiceID,
				tmplKey:     tmplKey,
			}
			n.coinserverWatchers[update.ServiceID] = cw
			cw.Start()
			log.Debug("New coinserver detected", "id", update.ServiceID[:8], "tmplKey", tmplKey)
		default:
			log.Warn("Unrecognized action from service watcher", "action", update.Action)
		}
	}
}

func (n *StratumServer) getBlockCast(key string) broadcast.Broadcaster {
	n.blockCastMtx.Lock()
	if _, ok := n.blockCast[key]; !ok {
		n.blockCast[key] = broadcast.NewBroadcaster(10)
	}
	n.blockCastMtx.Unlock()
	return n.blockCast[key]
}