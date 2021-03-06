// Copyright 2017 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package integration

import (
	"context"
	"crypto"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/golang/glog"
	ct "github.com/google/certificate-transparency-go"
	"github.com/google/certificate-transparency-go/client"
	"github.com/google/certificate-transparency-go/merkletree"
	"github.com/google/certificate-transparency-go/tls"
	"github.com/google/certificate-transparency-go/trillian/ctfe"
	"github.com/google/certificate-transparency-go/x509"
)

const defaultEmitSeconds = 10

// How many STHs and SCTs to hold on to.
const sthCount = 10
const sctCount = 10

// Maximum number of entries to request.
const maxEntriesCount = uint64(10)

var maxRetryDuration = 60 * time.Second

// errSkip indicates that a test operation should be skipped.
type errSkip struct{}

func (e errSkip) Error() string {
	return "test operation skipped"
}

// HammerConfig provides configuration for a stress/load test.
type HammerConfig struct {
	// Configuration for the log.
	LogCfg ctfe.LogConfig
	// Maximum merge delay.
	MMD time.Duration
	// Leaf certificate chain to use as template.
	LeafChain []ct.ASN1Cert
	// Parsed leaf certificate to use as template.
	LeafCert *x509.Certificate
	// Intermediate CA certificate chain to use as re-signing CA.
	CACert *x509.Certificate
	Signer crypto.Signer
	// ClientPool provides the clients used to make requests.
	ClientPool ClientPool
	// Bias values to favor particular log operations
	EPBias HammerBias
	// Number of operations to perform.
	Operations uint64
	// MaxParallelChains sets the upper limit for the number of parallel
	// add-*-chain requests to make when the biasing model says to perfom an add.
	MaxParallelChains int
	// EmitInterval defines how frequently stats are logged.
	EmitInterval time.Duration
	// IgnoreErrors controls whether a hammer run fails immediately on any error.
	IgnoreErrors bool
}

// HammerBias indicates the bias for selecting different log operations.
type HammerBias struct {
	Bias  map[ctfe.EntrypointName]int
	total int
}

// Choose randomly picks an operation to perform according to the biases.
func (hb HammerBias) Choose() ctfe.EntrypointName {
	if hb.total == 0 {
		for _, ep := range ctfe.Entrypoints {
			hb.total += hb.Bias[ep]
		}
	}
	which := rand.Intn(hb.total)
	for _, ep := range ctfe.Entrypoints {
		which -= hb.Bias[ep]
		if which < 0 {
			return ep
		}
	}
	panic("random choice out of range")
}

type submittedCert struct {
	leafData    []byte
	leafHash    [sha256.Size]byte
	sct         *ct.SignedCertificateTimestamp
	integrateBy time.Time
	precert     bool
}

// pendingCerts holds certificates that have been submitted that we want
// to check inclusion proofs for.  The array is ordered from oldest to
// most recent, but new entries are only appended when enough time has
// passed since the last append, so the SCTs that get checked are spread
// out across the MMD period.
type pendingCerts struct {
	mu    sync.Mutex
	certs [sctCount]*submittedCert
}

// tryAppendCert locks mu, checks whether it's possible to append the cert, and
// appends it if so.
func (pc *pendingCerts) tryAppendCert(now time.Time, mmd time.Duration, submitted *submittedCert) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.canAppend(now, mmd) {
		which := 0
		for ; which < sctCount; which++ {
			if pc.certs[which] == nil {
				break
			}
		}
		pc.certs[which] = submitted
	}
}

// canAppend checks whether a pending cert can be appended.
// It must be called with mu locked.
func (pc *pendingCerts) canAppend(now time.Time, mmd time.Duration) bool {
	if pc.certs[sctCount-1] != nil {
		return false // full already
	}
	if pc.certs[0] == nil {
		return true // nothing yet
	}
	// Only allow append if enough time has passed, namely MMD/#savedSCTs.
	last := sctCount - 1
	for ; last >= 0; last-- {
		if pc.certs[last] != nil {
			break
		}
	}
	lastTime := timeFromMS(pc.certs[last].sct.Timestamp)
	nextTime := lastTime.Add(mmd / sctCount)
	return now.After(nextTime)
}

// popIfMMDPassed returns the oldest submitted certificate (and removes it) if the
// maximum merge delay has passed, i.e. it is expected to be integrated as of now.
// This function locks mu.
func (pc *pendingCerts) popIfMMDPassed(now time.Time) *submittedCert {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.certs[0] == nil {
		return nil
	}
	submitted := pc.certs[0]
	if !now.After(submitted.integrateBy) {
		// Oldest cert not due to be integrated yet, so neither will any others.
		return nil
	}
	// Can pop the oldest cert and shuffle the others along, which make room for
	// another cert to be stored.
	for i := 0; i < (sctCount - 1); i++ {
		pc.certs[i] = pc.certs[i+1]
	}
	pc.certs[sctCount-1] = nil
	return submitted
}

// hammerState tracks the operations that have been performed during a test run, including
// earlier SCTs/STHs for later checking.
type hammerState struct {
	mu    sync.RWMutex
	cfg   *HammerConfig
	stats *logStats
	// STHs are arranged from later to earlier (so [0] is the most recent), and the
	// discovery of new STHs will push older ones off the end.
	sth [sthCount]*ct.SignedTreeHead
	// Submitted certs also run from later to earlier, but the discovery of new SCTs
	// does not affect the existing contents of the array, so if the array is full it
	// keeps the same elements.  Instead, the oldest entry is removed (and a space
	// created) when we are able to get an inclusion proof for it.
	pending pendingCerts
	// totalOps is a running count of operations performed by this hammer.
	totalOps int64
	// totalErrs is a running count of failed operations.
	totalErrs int64
}

func newHammerState(cfg *HammerConfig) (*hammerState, error) {
	if cfg.EmitInterval == 0 {
		cfg.EmitInterval = defaultEmitSeconds * time.Second
	}
	state := hammerState{
		cfg:   cfg,
		stats: newLogStats(cfg.LogCfg.LogID),
	}
	return &state, nil
}

func (s *hammerState) client() *client.LogClient {
	return s.cfg.ClientPool.Next()
}

// addMultiple calls the passed in function a random number
// (1 <= n < MaxParallelChains) of times.
// The first of any errors returned by calls to addOne will be returned by this function.
func (s *hammerState) addMultiple(ctx context.Context, addOne func(context.Context) error) error {
	var wg sync.WaitGroup
	numAdds := rand.Intn(s.cfg.MaxParallelChains) + 1
	errs := make(chan error, numAdds)
	for i := 0; i < numAdds; i++ {
		wg.Add(1)
		go func() {
			if err := addOne(ctx); err != nil {
				errs <- err
			}
			wg.Done()
		}()
	}
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
	}
	return nil
}

func (s *hammerState) addChain(ctx context.Context) error {
	chain, err := makeCertChain(s.cfg.LeafChain, s.cfg.LeafCert, s.cfg.CACert, s.cfg.Signer)
	if err != nil {
		return fmt.Errorf("failed to make fresh cert: %v", err)
	}
	sct, err := s.client().AddChain(ctx, chain)
	if err != nil {
		return fmt.Errorf("failed to add-chain: %v", err)
	}
	glog.V(2).Infof("%s: Uploaded cert, got SCT(time=%q)", s.cfg.LogCfg.Prefix, timeFromMS(sct.Timestamp))
	// Calculate leaf hash =  SHA256(0x00 | tls-encode(MerkleTreeLeaf))
	submitted := submittedCert{precert: false, sct: sct}
	leaf := ct.MerkleTreeLeaf{
		Version:  ct.V1,
		LeafType: ct.TimestampedEntryLeafType,
		TimestampedEntry: &ct.TimestampedEntry{
			Timestamp:  sct.Timestamp,
			EntryType:  ct.X509LogEntryType,
			X509Entry:  &(chain[0]),
			Extensions: sct.Extensions,
		},
	}
	submitted.integrateBy = timeFromMS(sct.Timestamp).Add(s.cfg.MMD)
	submitted.leafData, err = tls.Marshal(leaf)
	if err != nil {
		return fmt.Errorf("failed to tls.Marshal leaf cert: %v", err)
	}
	submitted.leafHash = sha256.Sum256(append([]byte{merkletree.LeafPrefix}, submitted.leafData...))
	s.pending.tryAppendCert(time.Now(), s.cfg.MMD, &submitted)
	glog.V(3).Infof("%s: Uploaded cert has leaf-hash %x", s.cfg.LogCfg.Prefix, submitted.leafHash)
	return nil
}

func (s *hammerState) addPreChain(ctx context.Context) error {
	prechain, tbs, err := makePrecertChain(s.cfg.LeafChain, s.cfg.LeafCert, s.cfg.CACert, s.cfg.Signer)
	if err != nil {
		return fmt.Errorf("failed to make fresh pre-cert: %v", err)
	}
	sct, err := s.client().AddPreChain(ctx, prechain)
	if err != nil {
		return fmt.Errorf("failed to add-pre-chain: %v", err)
	}
	glog.V(2).Infof("%s: Uploaded pre-cert, got SCT(time=%q)", s.cfg.LogCfg.Prefix, timeFromMS(sct.Timestamp))
	// Calculate leaf hash =  SHA256(0x00 | tls-encode(MerkleTreeLeaf))
	submitted := submittedCert{precert: true, sct: sct}
	leaf := ct.MerkleTreeLeaf{
		Version:  ct.V1,
		LeafType: ct.TimestampedEntryLeafType,
		TimestampedEntry: &ct.TimestampedEntry{
			Timestamp: sct.Timestamp,
			EntryType: ct.PrecertLogEntryType,
			PrecertEntry: &ct.PreCert{
				IssuerKeyHash:  sha256.Sum256(s.cfg.CACert.RawSubjectPublicKeyInfo),
				TBSCertificate: tbs,
			},
			Extensions: sct.Extensions,
		},
	}
	submitted.integrateBy = timeFromMS(sct.Timestamp).Add(s.cfg.MMD)
	submitted.leafData, err = tls.Marshal(leaf)
	if err != nil {
		return fmt.Errorf("tls.Marshal(precertLeaf)=(nil,%v); want (_,nil)", err)
	}
	submitted.leafHash = sha256.Sum256(append([]byte{merkletree.LeafPrefix}, submitted.leafData...))
	s.pending.tryAppendCert(time.Now(), s.cfg.MMD, &submitted)
	glog.V(3).Infof("%s: Uploaded pre-cert has leaf-hash %x", s.cfg.LogCfg.Prefix, submitted.leafHash)
	return nil
}

func (s *hammerState) getSTH(ctx context.Context) error {
	// Shuffle earlier STHs along.
	for i := sthCount - 1; i > 0; i-- {
		s.sth[i] = s.sth[i-1]
	}
	var err error
	s.sth[0], err = s.client().GetSTH(ctx)
	if err != nil {
		return fmt.Errorf("failed to get-sth: %v", err)
	}
	glog.V(2).Infof("%s: Got STH(time=%q, size=%d)", s.cfg.LogCfg.Prefix, timeFromMS(s.sth[0].Timestamp), s.sth[0].TreeSize)
	return nil
}

func (s *hammerState) getSTHConsistency(ctx context.Context) error {
	// Get current size, and pick an earlier size
	sthNow, err := s.client().GetSTH(ctx)
	if err != nil {
		return fmt.Errorf("failed to get-sth for current tree: %v", err)
	}
	which := rand.Intn(sthCount)
	if s.sth[which] == nil || s.sth[which].TreeSize == 0 {
		glog.V(3).Infof("%s: skipping get-sth-consistency as no earlier STH", s.cfg.LogCfg.Prefix)
		return errSkip{}
	}
	if s.sth[which].TreeSize == sthNow.TreeSize {
		glog.V(3).Infof("%s: skipping get-sth-consistency as same size (%d)", s.cfg.LogCfg.Prefix, sthNow.TreeSize)
		return errSkip{}
	}

	proof, err := s.client().GetSTHConsistency(ctx, s.sth[which].TreeSize, sthNow.TreeSize)
	if err != nil {
		return fmt.Errorf("failed to get-sth-consistency(%d, %d): %v", s.sth[which].TreeSize, sthNow.TreeSize, err)
	}
	if err := checkCTConsistencyProof(s.sth[which], sthNow, proof); err != nil {
		return fmt.Errorf("get-sth-consistency(%d, %d) proof check failed: %v", s.sth[which].TreeSize, sthNow.TreeSize, err)
	}
	glog.V(2).Infof("%s: Got STH consistency proof (size=%d => %d) len %d",
		s.cfg.LogCfg.Prefix, s.sth[which].TreeSize, sthNow.TreeSize, len(proof))
	return nil
}

func (s *hammerState) getProofByHash(ctx context.Context) error {
	submitted := s.pending.popIfMMDPassed(time.Now())
	if submitted == nil {
		// No SCT that is guaranteed to be integrated, so move on.
		return errSkip{}
	}
	// Get an STH that should include this submitted [pre-]cert.
	sth, err := s.client().GetSTH(ctx)
	if err != nil {
		return fmt.Errorf("failed to get-sth for proof: %v", err)
	}
	// Get and check an inclusion proof.
	rsp, err := s.client().GetProofByHash(ctx, submitted.leafHash[:], sth.TreeSize)
	if err != nil {
		return fmt.Errorf("failed to get-proof-by-hash(size=%d) on cert with SCT @ %v: %v, %+v", sth.TreeSize, timeFromMS(submitted.sct.Timestamp), err, rsp)
	}
	if err := Verifier.VerifyInclusionProof(rsp.LeafIndex, int64(sth.TreeSize), rsp.AuditPath, sth.SHA256RootHash[:], submitted.leafData); err != nil {
		return fmt.Errorf("failed to VerifyInclusionProof(%d, %d)=%v", rsp.LeafIndex, sth.TreeSize, err)
	}
	return nil
}

func (s *hammerState) getEntries(ctx context.Context) error {
	if s.sth[0] == nil || s.sth[0].TreeSize == 0 {
		glog.V(3).Infof("%s: skipping get-entries as no earlier STH", s.cfg.LogCfg.Prefix)
		return errSkip{}
	}
	count := uint64(1 + rand.Intn(int(maxEntriesCount-1)))
	if count > s.sth[0].TreeSize {
		count = s.sth[0].TreeSize
	}
	// Entry indices are zero-based.
	first := int64(s.sth[0].TreeSize - count)
	last := int64(s.sth[0].TreeSize) - 1
	entries, err := s.client().GetEntries(ctx, first, last)
	if err != nil {
		return fmt.Errorf("failed to get-entries(%d,%d): %v", first, last, err)
	}
	if len(entries) < int(count) {
		return fmt.Errorf("get-entries(%d,%d) returned %d entries; want %d", first, last, len(entries), count)
	}
	for i, entry := range entries {
		leaf := entry.Leaf
		ts := leaf.TimestampedEntry
		if leaf.Version != 0 {
			return fmt.Errorf("leaf[%d].Version=%v; want V1(0)", i, leaf.Version)
		}
		if leaf.LeafType != ct.TimestampedEntryLeafType {
			return fmt.Errorf("leaf[%d].Version=%v; want TimestampedEntryLeafType", i, leaf.LeafType)
		}
		if ts.EntryType != ct.X509LogEntryType && ts.EntryType != ct.PrecertLogEntryType {
			return fmt.Errorf("leaf[%d].ts.EntryType=%v; want {X509,Precert}LogEntryType", i, ts.EntryType)
		}
	}
	glog.V(2).Infof("%s: Got entries [%d:%d+1]\n", s.cfg.LogCfg.Prefix, first, last)
	return nil
}

func (s *hammerState) getRoots(ctx context.Context) error {
	roots, err := s.client().GetAcceptedRoots(ctx)
	if err != nil {
		return fmt.Errorf("failed to get-roots: %v", err)
	}
	glog.V(2).Infof("%s: Got roots (len=%d)", s.cfg.LogCfg.Prefix, len(roots))
	return nil
}

func sthSize(sth *ct.SignedTreeHead) string {
	if sth == nil {
		return "n/a"
	}
	return fmt.Sprintf("%d", sth.TreeSize)
}

func (s *hammerState) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	l := fmt.Sprintf("%10s: lastSTH.size=%s ops: total=%d errs=%d", s.cfg.LogCfg.Prefix, sthSize(s.sth[0]), s.totalOps, s.totalErrs)
	statusOK := strconv.Itoa(http.StatusOK)
	for _, ep := range ctfe.Entrypoints {
		if s.cfg.EPBias.Bias[ep] > 0 {
			l += fmt.Sprintf(" %s=%d/%d", ep, s.stats.rsps[string(ep)][statusOK], s.stats.reqs[string(ep)])
		}
	}
	return l
}

func isSkip(e error) bool {
	_, ok := e.(errSkip)
	return ok
}

func (s *hammerState) performOp(ctx context.Context, ep ctfe.EntrypointName) (int, error) {
	status := http.StatusOK
	var err error
	switch ep {
	case ctfe.AddChainName:
		err = s.addMultiple(ctx, s.addChain)
	case ctfe.AddPreChainName:
		err = s.addMultiple(ctx, s.addPreChain)
	case ctfe.GetSTHName:
		err = s.getSTH(ctx)
	case ctfe.GetSTHConsistencyName:
		err = s.getSTHConsistency(ctx)
	case ctfe.GetProofByHashName:
		err = s.getProofByHash(ctx)
	case ctfe.GetEntriesName:
		err = s.getEntries(ctx)
	case ctfe.GetRootsName:
		err = s.getRoots(ctx)
	case ctfe.GetEntryAndProofName:
		status = http.StatusNotImplemented
		glog.V(2).Infof("%s: hammering entrypoint %s not yet implemented", s.cfg.LogCfg.Prefix, ep)
	default:
		err = fmt.Errorf("internal error: unknown entrypoint %s selected", ep)
	}
	return status, err
}

func (s *hammerState) chooseOp() ctfe.EntrypointName {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.EPBias.Choose()
}

func (s *hammerState) retryOneOp(ctx context.Context) (err error) {
	ep := s.chooseOp()
	glog.V(3).Infof("perform %s operation", ep)

	status := http.StatusOK
	deadline := time.Now().Add(maxRetryDuration)

	done := false
	for !done {
		s.mu.Lock()

		s.totalOps++

		status, err = s.performOp(ctx, ep)

		switch err.(type) {
		case nil:
			s.stats.done(ep, status)
			done = true
		case errSkip:
			status = http.StatusFailedDependency
			err = nil
			done = true
		default:
			s.totalErrs++
			if s.cfg.IgnoreErrors {
				glog.Warningf("%s: op %v failed (will retry): %v", s.cfg.LogCfg.Prefix, ep, err)
			} else {
				done = true
			}
		}

		s.mu.Unlock()

		if time.Now().After(deadline) {
			glog.Warningf("%s: gave up retrying failed op %v after %v", s.cfg.LogCfg.Prefix, ep, maxRetryDuration)
			done = true
		}
	}
	return err
}

// HammerCTLog performs load/stress operations according to given config.
func HammerCTLog(cfg HammerConfig) error {
	s, err := newHammerState(&cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()
	ticker := time.NewTicker(cfg.EmitInterval)

	go func(c <-chan time.Time) {
		for _ = range c {
			glog.Info(s.String())
		}
	}(ticker.C)

	for count := uint64(1); count < cfg.Operations; count++ {
		if err := s.retryOneOp(ctx); err != nil {
			return err
		}
	}
	ticker.Stop()

	return nil
}
