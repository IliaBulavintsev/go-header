package sync

import (
	"context"
	"testing"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/celestiaorg/go-header"
	"github.com/celestiaorg/go-header/headertest"
	"github.com/celestiaorg/go-header/local"
	"github.com/celestiaorg/go-header/store"
)

func TestSyncSimpleRequestingHead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	t.Cleanup(cancel)

	suite := headertest.NewTestSuite(t)
	head := suite.Head()

	remoteStore := store.NewTestStore(ctx, t, head)
	err := remoteStore.Append(ctx, suite.GenDummyHeaders(100)...)
	require.NoError(t, err)

	_, err = remoteStore.GetByHeight(ctx, 100)
	require.NoError(t, err)

	localStore := store.NewTestStore(ctx, t, head)
	syncer, err := NewSyncer[*headertest.DummyHeader](
		local.NewExchange(remoteStore),
		localStore,
		headertest.NewDummySubscriber(),
		WithBlockTime(time.Second*30),
		WithTrustingPeriod(time.Microsecond),
	)
	require.NoError(t, err)
	err = syncer.Start(ctx)
	require.NoError(t, err)

	time.Sleep(time.Millisecond * 10) // needs some to realize it is syncing
	err = syncer.SyncWait(ctx)
	require.NoError(t, err)

	exp, err := remoteStore.Head(ctx)
	require.NoError(t, err)

	have, err := localStore.Head(ctx)
	require.NoError(t, err)
	assert.Equal(t, exp.Height(), have.Height())
	assert.Empty(t, syncer.pending.Head())

	state := syncer.State()
	assert.Equal(t, uint64(exp.Height()), state.Height)
	assert.Equal(t, uint64(2), state.FromHeight)
	assert.Equal(t, uint64(exp.Height()), state.ToHeight)
	assert.True(t, state.Finished(), state)
}

func TestDoSyncFullRangeFromExternalPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	t.Cleanup(cancel)

	suite := headertest.NewTestSuite(t)
	head := suite.Head()

	remoteStore := store.NewTestStore(ctx, t, head)
	localStore := store.NewTestStore(ctx, t, head)
	syncer, err := NewSyncer[*headertest.DummyHeader](
		local.NewExchange(remoteStore),
		localStore,
		headertest.NewDummySubscriber(),
	)
	require.NoError(t, err)
	require.NoError(t, syncer.Start(ctx))

	err = remoteStore.Append(ctx, suite.GenDummyHeaders(int(header.MaxRangeRequestSize))...)
	require.NoError(t, err)
	// give store time to update heightSub index
	time.Sleep(time.Millisecond * 100)

	// trigger sync by calling Head
	_, err = syncer.Head(ctx)
	require.NoError(t, err)

	// give store time to sync
	time.Sleep(time.Millisecond * 100)

	remoteHead, err := remoteStore.Head(ctx)
	require.NoError(t, err)

	newHead, err := localStore.Head(ctx)
	require.NoError(t, err)
	require.Equal(t, newHead.Height(), remoteHead.Height())
}

func TestSyncCatchUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	t.Cleanup(cancel)

	suite := headertest.NewTestSuite(t)
	head := suite.Head()

	remoteStore := store.NewTestStore(ctx, t, head)
	localStore := store.NewTestStore(ctx, t, head)
	syncer, err := NewSyncer[*headertest.DummyHeader](
		local.NewExchange(remoteStore),
		localStore,
		headertest.NewDummySubscriber(),
		WithTrustingPeriod(time.Minute),
	)
	require.NoError(t, err)
	// 1. Initial sync
	err = syncer.Start(ctx)
	require.NoError(t, err)

	// 2. chain grows and syncer misses that
	err = remoteStore.Append(ctx, suite.GenDummyHeaders(100)...)
	require.NoError(t, err)

	incomingHead := suite.GenDummyHeaders(1)[0]
	// 3. syncer rcvs header from the future and starts catching-up
	res := syncer.incomingNetworkHead(ctx, incomingHead)
	assert.Equal(t, pubsub.ValidationAccept, res)

	time.Sleep(time.Millisecond * 100) // needs some to realize it is syncing
	err = syncer.SyncWait(ctx)
	require.NoError(t, err)

	exp, err := remoteStore.Head(ctx)
	require.NoError(t, err)

	// 4. assert syncer caught-up
	have, err := localStore.Head(ctx)
	require.NoError(t, err)

	assert.Equal(t, have.Height(), incomingHead.Height())
	assert.Equal(t, exp.Height()+1, have.Height()) // plus one as we didn't add last header to remoteStore
	assert.Empty(t, syncer.pending.Head())

	state := syncer.State()
	assert.Equal(t, uint64(exp.Height()+1), state.Height)
	assert.Equal(t, uint64(2), state.FromHeight)
	assert.Equal(t, uint64(exp.Height()+1), state.ToHeight)
	assert.True(t, state.Finished(), state)
}

func TestSyncPendingRangesWithMisses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	t.Cleanup(cancel)

	suite := headertest.NewTestSuite(t)
	head := suite.Head()

	remoteStore := store.NewTestStore(ctx, t, head)
	localStore := store.NewTestStore(ctx, t, head)
	syncer, err := NewSyncer[*headertest.DummyHeader](
		local.NewExchange(remoteStore),
		localStore,
		headertest.NewDummySubscriber(),
		WithTrustingPeriod(time.Minute),
	)
	require.NoError(t, err)
	err = syncer.Start(ctx)
	require.NoError(t, err)

	// miss 1 (helps to test that Syncer properly requests missed Headers from Exchange)
	err = remoteStore.Append(ctx, suite.GenDummyHeaders(1)...)
	require.NoError(t, err)

	range1 := suite.GenDummyHeaders(15)
	err = remoteStore.Append(ctx, range1...)
	require.NoError(t, err)

	// miss 2
	err = remoteStore.Append(ctx, suite.GenDummyHeaders(3)...)
	require.NoError(t, err)

	range2 := suite.GenDummyHeaders(23)
	err = remoteStore.Append(ctx, range2...)
	require.NoError(t, err)

	// manually add to pending
	for _, h := range append(range1, range2...) {
		syncer.pending.Add(h)
	}

	// and fire up a sync
	syncer.sync(ctx)

	_, err = remoteStore.GetByHeight(ctx, 43)
	require.NoError(t, err)
	_, err = localStore.GetByHeight(ctx, 43)
	require.NoError(t, err)

	lastHead, err := syncer.store.Head(ctx)
	require.NoError(t, err)
	require.Equal(t, lastHead.Height(), int64(43))
	exp, err := remoteStore.Head(ctx)
	require.NoError(t, err)

	have, err := localStore.Head(ctx)
	require.NoError(t, err)

	assert.Equal(t, exp.Height(), have.Height())
	assert.Empty(t, syncer.pending.Head()) // assert all cache from pending is used
}

// TestSyncer_FindHeadersReturnsCorrectRange ensures that `findHeaders` returns
// range [from;to]
func TestSyncer_FindHeadersReturnsCorrectRange(t *testing.T) {
	// Test consists of 3 steps:
	// 1. get range of headers from pending; [2;11]
	// 2. get headers from the remote store; [12;20]
	// 3. apply last header from pending;
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	t.Cleanup(cancel)

	suite := headertest.NewTestSuite(t)
	head := suite.Head()

	remoteStore := store.NewTestStore(ctx, t, head)
	localStore := store.NewTestStore(ctx, t, head)
	syncer, err := NewSyncer[*headertest.DummyHeader](
		local.NewExchange(remoteStore),
		localStore,
		headertest.NewDummySubscriber(),
	)
	require.NoError(t, err)

	range1 := suite.GenDummyHeaders(10)
	// manually add to pending
	for _, h := range range1 {
		syncer.pending.Add(h)
	}
	err = remoteStore.Append(ctx, range1...)
	require.NoError(t, err)
	err = remoteStore.Append(ctx, suite.GenDummyHeaders(9)...)
	require.NoError(t, err)

	syncer.pending.Add(suite.NextHeader())
	require.NoError(t, err)
	err = syncer.processHeaders(ctx, head, 21)
	require.NoError(t, err)

	head, err = syncer.store.Head(ctx)
	require.NoError(t, err)
	assert.Equal(t, head.Height(), int64(21))
}

func TestSyncerIncomingDuplicate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	t.Cleanup(cancel)

	suite := headertest.NewTestSuite(t)
	head := suite.Head()

	remoteStore := store.NewTestStore(ctx, t, head)
	localStore := store.NewTestStore(ctx, t, head)
	syncer, err := NewSyncer[*headertest.DummyHeader](
		&delayedGetter[*headertest.DummyHeader]{Getter: local.NewExchange(remoteStore)},
		localStore,
		headertest.NewDummySubscriber(),
	)
	require.NoError(t, err)
	err = syncer.Start(ctx)
	require.NoError(t, err)

	range1 := suite.GenDummyHeaders(10)
	err = remoteStore.Append(ctx, range1...)
	require.NoError(t, err)

	res := syncer.incomingNetworkHead(ctx, range1[len(range1)-1])
	assert.Equal(t, pubsub.ValidationAccept, res)

	time.Sleep(time.Millisecond * 10)

	res = syncer.incomingNetworkHead(ctx, range1[len(range1)-1])
	assert.Equal(t, pubsub.ValidationIgnore, res)

	err = syncer.SyncWait(ctx)
	require.NoError(t, err)
}

// TestSync_InvalidSyncTarget tests the possible case that an incoming
// header passes non-adjacent verification and is set as the sync target
// but is actually invalid once it is processed via VerifyAdjacent during sync
func TestSync_InvalidSyncTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	t.Cleanup(cancel)

	suite := headertest.NewTestSuite(t)
	head := suite.Head()

	remoteStore := headertest.NewStore[*headertest.DummyHeader](t, suite, 100)
	localStore := store.NewTestStore(ctx, t, head)

	syncer, err := NewSyncer[*headertest.DummyHeader](
		local.NewExchange[*headertest.DummyHeader](remoteStore),
		localStore,
		headertest.NewDummySubscriber(),
		WithTrustingPeriod(time.Second),
		WithBlockTime(time.Nanosecond),
	)
	require.NoError(t, err)

	headers := suite.GenDummyHeaders(300)
	// malform the remote store's head so that it can serve
	// the syncer a "bad" sync target that passes initial validation,
	// but not verification.
	maliciousHeader := headers[299]
	maliciousHeader.VerifyFailure = true
	err = remoteStore.Append(ctx, headers...)
	require.NoError(t, err)

	// TODO
	h, err := syncer.Head(ctx)
	require.NoError(t, err)
	require.Equal(t, maliciousHeader.Height(), h.Height())

	err = syncer.Start(ctx)
	require.NoError(t, err)

	time.Sleep(time.Millisecond * 100) // TODO flakey?

	shortCtx, cancel := context.WithTimeout(ctx, time.Millisecond*200)
	err = syncer.SyncWait(shortCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	cancel()

	// TODO
	require.Equal(t, uint64(maliciousHeader.Height()), syncer.State().ToHeight)

	h, err = localStore.Head(ctx)
	require.NoError(t, err)
	require.Equal(t, maliciousHeader.Height()-1, h.Height())

	// TODO
	remoteStore.Headers[maliciousHeader.Height()].VerifyFailure = false

	// generate more headers and trigger sync again
	err = remoteStore.Append(ctx, suite.GenDummyHeaders(100)...)
	require.NoError(t, err)

	// pretend new header is received from network
	expectedHead, err := remoteStore.Head(ctx)
	require.NoError(t, err)
	syncer.incomingNetworkHead(ctx, expectedHead)
	err = syncer.SyncWait(ctx)
	require.NoError(t, err)

	// ensure that maliciousHeader height was re-requested and a good one was
	// found
	rerequested, err := localStore.GetByHeight(ctx, uint64(maliciousHeader.Height()))
	require.NoError(t, err)
	require.False(t, rerequested.VerifyFailure)

	gotHead, err := localStore.Head(ctx)
	require.NoError(t, err)

	syncHead, err := syncer.Head(ctx)
	require.NoError(t, err)

	require.Equal(t, expectedHead.Height()-1, gotHead.Height())
	require.Equal(t, expectedHead.Height(), syncHead.Height())
}

type delayedGetter[H header.Header] struct {
	header.Getter[H]
}

func (d *delayedGetter[H]) GetVerifiedRange(ctx context.Context, from H, amount uint64) ([]H, error) {
	select {
	case <-time.After(time.Millisecond * 100):
		return d.Getter.GetVerifiedRange(ctx, from, amount)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
