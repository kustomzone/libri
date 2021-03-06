package server

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"

	"github.com/drausin/libri/libri/common/db"
	"github.com/drausin/libri/libri/common/ecid"
	cid "github.com/drausin/libri/libri/common/id"
	"github.com/drausin/libri/libri/common/storage"
	"github.com/drausin/libri/libri/common/subscribe"
	"github.com/drausin/libri/libri/librarian/api"
	"github.com/drausin/libri/libri/librarian/client"
	"github.com/drausin/libri/libri/librarian/server/introduce"
	"github.com/drausin/libri/libri/librarian/server/peer"
	"github.com/drausin/libri/libri/librarian/server/routing"
	"github.com/drausin/libri/libri/librarian/server/search"
	"github.com/drausin/libri/libri/librarian/server/store"
	"github.com/willf/bloom"
	"go.uber.org/zap"
	"golang.org/x/net/context"
	"google.golang.org/grpc/health"
)

// Librarian is the main service of a single peer in the peer to peer network.
type Librarian struct {
	// SelfID is the random 256-bit identification number of this node in the hash table
	selfID ecid.ID

	// Config holds the configuration parameters of the server
	config *Config

	// fixed API address
	apiSelf *api.PeerAddress

	// executes introductions to peers
	introducer introduce.Introducer

	// executes searches for peers and keys
	searcher search.Searcher

	// executes stores for key/value
	storer store.Storer

	// manages subscriptions from other peers
	subscribeFrom subscribe.From

	// manages subscriptions to other peers
	subscribeTo subscribe.To

	// RecentPubs is an LRU cache of recent publications librarian has received
	RecentPubs subscribe.RecentPublications

	// verifies requests from peers
	rqv RequestVerifier

	// key-value store DB used for all external storage
	db db.KVDB

	// SL for server data
	serverSL storage.NamespaceStorerLoader

	// SL for p2p stored documents
	documentSL storage.DocumentStorerLoader

	// ensures keys are valid
	kc storage.Checker

	// ensures keys and values are valid
	kvc storage.KeyValueChecker

	// creates new peers
	fromer peer.Fromer

	// signs requests
	signer client.Signer

	// routing table of peers
	rt routing.Table

	// logger for this instance
	logger *zap.Logger

	// health server
	health *health.Server

	// receives graceful stop signal
	stop chan struct{}
}

var newPublicationsSlack = 16

// NewLibrarian creates a new librarian instance.
func NewLibrarian(config *Config, logger *zap.Logger) (*Librarian, error) {
	rdb, err := db.NewRocksDB(config.DbDir)
	if err != nil {
		logger.Error("unable to init RocksDB", zap.Error(err))
		return nil, err
	}
	serverSL := storage.NewServerKVDBStorerLoader(rdb)
	documentSL := storage.NewDocumentKVDBStorerLoader(rdb)

	// get peer ID and immediately save it so subsequent restarts have it
	peerID, err := loadOrCreatePeerID(logger, serverSL)
	if err != nil {
		return nil, err
	}

	rt, err := loadOrCreateRoutingTable(logger, serverSL, peerID, config.Routing)
	if err != nil {
		return nil, err
	}

	signer := client.NewSigner(peerID.Key())
	searcher := search.NewDefaultSearcher(signer)
	newPubs := make(chan *subscribe.KeyedPub, newPublicationsSlack)

	recentPubs, err := subscribe.NewRecentPublications(config.SubscribeTo.RecentCacheSize)
	if err != nil {
		return nil, err
	}
	clientBalancer := routing.NewClientBalancer(rt)
	subscribeTo := subscribe.NewTo(config.SubscribeTo, logger, peerID, clientBalancer, signer,
		recentPubs, newPubs)

	return &Librarian{
		selfID:        peerID,
		config:        config,
		apiSelf:       api.FromAddress(peerID.ID(), config.PublicName, config.PublicAddr),
		introducer:    introduce.NewDefaultIntroducer(signer, peerID.ID()),
		searcher:      searcher,
		storer:        store.NewStorer(signer, searcher, client.NewStoreQuerier()),
		subscribeFrom: subscribe.NewFrom(config.SubscribeFrom, logger, newPubs),
		subscribeTo:   subscribeTo,
		RecentPubs:    recentPubs,
		rqv:           NewRequestVerifier(),
		db:            rdb,
		serverSL:      serverSL,
		documentSL:    documentSL,
		kc:            storage.NewExactLengthChecker(storage.EntriesKeyLength),
		kvc:           storage.NewHashKeyValueChecker(),
		fromer:        peer.NewFromer(),
		signer:        signer,
		rt:            rt,
		logger:        logger,
		health:        health.NewServer(),
		stop:          make(chan struct{}),
	}, nil
}

// Ping confirms simple request/response connectivity.
func (l *Librarian) Ping(ctx context.Context, rq *api.PingRequest) (*api.PingResponse, error) {
	return &api.PingResponse{Message: "pong"}, nil
}

// Introduce receives and gives identifying information about the peer in the network.
func (l *Librarian) Introduce(ctx context.Context, rq *api.IntroduceRequest) (
	*api.IntroduceResponse, error) {
	l.logger.Debug("received introduce request")

	// check request
	requesterID, err := l.checkRequest(ctx, rq, rq.Metadata)
	if err != nil {
		l.logger.Debug("check request error", zap.String("error", err.Error()))
		return nil, err
	}
	requester := l.fromer.FromAPI(rq.Self)
	if requester.ID().Cmp(requesterID) != 0 {
		return nil, errors.New("stated client peer ID does not match signature")
	}
	l.record(requesterID, peer.Request, peer.Success)

	// add peer to routing table (if space)
	l.rt.Push(requester)

	// get random peers for client, using request ID as unique source of entropy for sample
	seed := int64(binary.BigEndian.Uint64(rq.Metadata.RequestId[:8]))
	peers := l.rt.Sample(uint(rq.NumPeers), rand.New(rand.NewSource(seed)))

	l.logger.Info("introduced",
		zap.String("self_id", l.selfID.String()),
		zap.Int("n_peers", len(peers)),
	)
	return &api.IntroduceResponse{
		Metadata: l.NewResponseMetadata(rq.Metadata),
		Self:     l.apiSelf,
		Peers:    peer.ToAPIs(peers),
	}, nil
}

// Find returns either the value at a given target or the peers closest to it.
func (l *Librarian) Find(ctx context.Context, rq *api.FindRequest) (*api.FindResponse, error) {
	requesterID, err := l.checkRequestAndKey(ctx, rq, rq.Metadata, rq.Key)
	if err != nil {
		return nil, err
	}
	l.record(requesterID, peer.Request, peer.Success)

	value, err := l.documentSL.Load(cid.FromBytes(rq.Key))
	if err != nil {
		// something went wrong during load
		return nil, err
	}

	// we have the value, so return it
	if value != nil {
		return &api.FindResponse{
			Metadata: l.NewResponseMetadata(rq.Metadata),
			Value:    value,
		}, nil
	}

	// otherwise, return the peers closest to the key
	key := cid.FromBytes(rq.Key)
	closest := l.rt.Peak(key, uint(rq.NumPeers))
	return &api.FindResponse{
		Metadata: l.NewResponseMetadata(rq.Metadata),
		Peers:    peer.ToAPIs(closest),
	}, nil
}

// Store stores the value.
func (l *Librarian) Store(ctx context.Context, rq *api.StoreRequest) (
	*api.StoreResponse, error) {
	requesterID, err := l.checkRequestAndKeyValue(ctx, rq, rq.Metadata, rq.Key, rq.Value)
	if err != nil {
		return nil, err
	}
	l.record(requesterID, peer.Request, peer.Success)

	if err := l.documentSL.Store(cid.FromBytes(rq.Key), rq.Value); err != nil {
		return nil, err
	}
	if err := l.subscribeTo.Send(api.GetPublication(rq.Key, rq.Value)); err != nil {
		return nil, err
	}

	l.logger.Debug("stored",
		zap.String("key", fmt.Sprintf("032%x", rq.Key)),
		zap.String("request_id", fmt.Sprintf("032%x", rq.Metadata.RequestId)),
		zap.String("self_id", l.selfID.String()),
	)
	return &api.StoreResponse{
		Metadata: l.NewResponseMetadata(rq.Metadata),
	}, nil
}

// Get returns the value for a given key, if it exists. This endpoint handles the internals of
// searching for the key.
func (l *Librarian) Get(ctx context.Context, rq *api.GetRequest) (*api.GetResponse, error) {
	requesterID, err := l.checkRequestAndKey(ctx, rq, rq.Metadata, rq.Key)
	if err != nil {
		return nil, err
	}
	l.record(requesterID, peer.Request, peer.Success)

	key := cid.FromBytes(rq.Key)
	s := search.NewSearch(l.selfID, key, l.config.Search)
	seeds := l.rt.Peak(key, s.Params.Concurrency)
	err = l.searcher.Search(s, seeds)
	if err != nil {
		return nil, err
	}

	// add found peers to routing table
	for _, p := range s.Result.Closest.Peers() {
		l.rt.Push(p)
	}

	if s.FoundValue() {
		// return the value found by the search
		l.logger.Info("got value", zap.String("key", key.String()))
		return &api.GetResponse{
			Metadata: l.NewResponseMetadata(rq.Metadata),
			Value:    s.Result.Value,
		}, nil
	}
	if s.FoundClosestPeers() {
		// return the nil value, indicating that the value wasn't found
		l.logger.Info("did not get value", zap.String("key", key.String()))
		return &api.GetResponse{
			Metadata: l.NewResponseMetadata(rq.Metadata),
			Value:    nil,
		}, nil
	}
	if s.Errored() {
		return nil, errors.New("search for key errored")
	}
	if s.Exhausted() {
		return nil, errors.New("search for key exhausted")
	}

	return nil, errors.New("unexpected search result")
}

// Put stores a given key and value. This endpoint handles the internals of finding the right
// peers to store the value in and then sending them store requests.
func (l *Librarian) Put(ctx context.Context, rq *api.PutRequest) (*api.PutResponse, error) {
	requesterID, err := l.checkRequestAndKeyValue(ctx, rq, rq.Metadata, rq.Key, rq.Value)
	if err != nil {
		return nil, err
	}
	l.record(requesterID, peer.Request, peer.Success)

	key := cid.FromBytes(rq.Key)
	s := store.NewStore(
		l.selfID,
		key,
		rq.Value,
		l.config.Search,
		l.config.Store,
	)
	seeds := l.rt.Peak(key, s.Search.Params.Concurrency)
	err = l.storer.Store(s, seeds)
	if err != nil {
		return nil, err
	}
	debugLogStoreResult("store result", s, l.logger)
	for _, p := range s.Result.Responded {
		l.rt.Push(p)
	}
	if s.Stored() {
		l.logger.Info("put value",
			zap.String("key", key.String()),
			zap.String("operation", api.PutOperation_STORED.String()),
		)
		return &api.PutResponse{
			Metadata:  l.NewResponseMetadata(rq.Metadata),
			Operation: api.PutOperation_STORED,
			NReplicas: uint32(len(s.Result.Responded)),
		}, nil
	}
	if s.Exists() {
		l.logger.Info("put value",
			zap.String("key", key.String()),
			zap.String("operation", api.PutOperation_LEFT_EXISTING.String()),
		)
		return &api.PutResponse{
			Metadata:  l.NewResponseMetadata(rq.Metadata),
			Operation: api.PutOperation_LEFT_EXISTING,
			NReplicas: uint32(len(s.Result.Responded)),
		}, nil
	}
	if s.Errored() {
		// TODO (drausin) better collect and surface errors from queries
		return nil, errors.New("received error during search or store operations")
	}
	if s.Exhausted() {
		return nil, errors.New("store for key exhausted")
	}

	return nil, fmt.Errorf("unexpected store result: %v", s.Result)
}

func debugLogSearchResult(message string, s *search.Search, logger *zap.Logger) {
	logger.Debug(message,
		zap.Bool("finished", s.Finished()),
		zap.Bool("errored", s.Errored()),
		zap.Bool("exhausted", s.Exhausted()),
		zap.Bool("found_value", s.FoundValue()),
		zap.Bool("found_closest_peers", s.FoundClosestPeers()),
		zap.Int("n_closest", s.Result.Closest.Len()),
		zap.Int("n_unqueried", s.Result.Unqueried.Len()),
		zap.Int("n_responded", len(s.Result.Responded)),
		zap.Int("n_errors", len(s.Result.Errored)),
		zap.Uint("param_n_closest_responses", s.Params.NClosestResponses),
	)
}

func debugLogStoreResult(message string, s *store.Store, logger *zap.Logger) {
	debugLogSearchResult(fmt.Sprintf("search %s", message), s.Search, logger)
	logger.Debug(message,
		zap.Bool("finished", s.Finished()),
		zap.Bool("stored", s.Stored()),
		zap.Bool("errored", s.Errored()),
		zap.Bool("exhausted", s.Exhausted()),
		zap.Bool("exists", s.Exists()),
		zap.Int("n_unqueried", len(s.Result.Unqueried)),
		zap.Int("n_responded", len(s.Result.Responded)),
		zap.Uint("n_errors", s.Result.NErrors),
	)
}

// Subscribe begins a subscription to the peer's publication stream (from its own subscriptions to
// other peers).
func (l *Librarian) Subscribe(rq *api.SubscribeRequest, from api.Librarian_SubscribeServer) error {
	if _, err := l.checkRequest(from.Context(), rq, rq.Metadata); err != nil {
		return err
	}
	authorFilter, err := subscribe.FromAPI(rq.Subscription.AuthorPublicKeys)
	if err != nil {
		return err
	}
	readerFilter, err := subscribe.FromAPI(rq.Subscription.ReaderPublicKeys)
	if err != nil {
		return err
	}
	pubs, done, err := l.subscribeFrom.New()
	if err != nil {
		return err
	}

	responseMetadata := l.NewResponseMetadata(rq.Metadata)
	for pub := range pubs {
		err = l.maybeSend(pub, authorFilter, readerFilter, from, responseMetadata, done)
		if err != nil {
			return err
		}
	}

	// only close done if it's not already
	select {
	case <-done:
	default:
		close(done)
	}
	return nil
}

func (l *Librarian) maybeSend(
	pub *subscribe.KeyedPub,
	authorFilter *bloom.BloomFilter,
	readerFilter *bloom.BloomFilter,
	from api.Librarian_SubscribeServer,
	responseMetadata *api.ResponseMetadata,
	done chan struct{},
) error {

	if !authorFilter.Test(pub.Value.AuthorPublicKey) {
		return nil
	}
	if !readerFilter.Test(pub.Value.ReaderPublicKey) {
		return nil
	}

	// if we get to here, we know that both author and reader keys are in the filters,
	// so we want to send the response
	rp := &api.SubscribeResponse{
		Metadata: responseMetadata,
		Key:      pub.Key.Bytes(),
		Value:    pub.Value,
	}
	if err := from.Send(rp); err != nil {
		l.logger.Error("subscribe send error", zap.Error(err))
		close(done) // signal to l.subscribeFrom we're finished with this fanout
		return err
	}
	l.logger.Debug("sent publication", zap.String("publication_key", pub.Key.String()))
	return nil
}
