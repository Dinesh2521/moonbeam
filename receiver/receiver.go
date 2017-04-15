package receiver

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcrpcclient"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/hdkeychain"

	"bitbucket.org/bitx/moonchan/channels"
	"bitbucket.org/bitx/moonchan/models"
	"bitbucket.org/bitx/moonchan/storage"
)

// Policy parameters
const (
	softTimeout    = 144
	fundingMinConf = 3
)

type Receiver struct {
	Net            *chaincfg.Params
	ek             *hdkeychain.ExtendedKey
	bc             *btcrpcclient.Client
	db             storage.Storage
	dir            *Directory
	receiverOutput string
	config         channels.ReceiverConfig
}

func NewReceiver(net *chaincfg.Params,
	ek *hdkeychain.ExtendedKey,
	bc *btcrpcclient.Client,
	db storage.Storage,
	dir *Directory,
	destination string) *Receiver {

	config := channels.DefaultReceiverConfig
	config.Net = net.Name

	return &Receiver{
		Net:            net,
		ek:             ek,
		bc:             bc,
		db:             db,
		dir:            dir,
		receiverOutput: destination,
		config:         config,
	}
}

func (r *Receiver) Get(id string) *channels.SharedState {
	rec, err := r.db.Get(id)
	if err != nil {
		return nil
	}
	if rec == nil {
		return nil
	}
	return &rec.SharedState
}

func (r *Receiver) List() ([]storage.Record, error) {
	return r.db.List()
}

func (r *Receiver) ListPayments(channelID string) ([][]byte, error) {
	return r.db.ListPayments(channelID)
}

func (r *Receiver) getKey(n int) (*btcec.PrivateKey, error) {
	ek, err := r.ek.Child(uint32(n))
	if err != nil {
		return nil, err
	}
	return ek.ECPrivKey()
}

func genChannelID() (string, error) {
	buf := make([]byte, 32)
	_, err := rand.Read(buf)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (r *Receiver) Create(req models.CreateRequest) (*models.CreateResponse, error) {
	// channels.Receiver.Create would validate these fields, but we do it
	// here anyway because we have to hit the db beforehand.
	if _, err := btcutil.NewAddressPubKey(req.SenderPubKey, r.Net); err != nil {
		return nil, err
	}
	if req.Version != channels.Version {
		return nil, errors.New("unsupported version")
	}
	if req.Net != r.config.Net {
		return nil, errors.New("unsupported network")
	}

	n, err := r.db.ReserveKeyPath()
	if err != nil {
		return nil, err
	}
	privKey, err := r.getKey(n)
	if err != nil {
		return nil, err
	}

	c, err := channels.NewReceiver(r.config, privKey)
	if err != nil {
		return nil, err
	}
	resp, err := c.Create(r.receiverOutput, &req)
	if err != nil {
		return nil, err
	}

	id, err := genChannelID()
	if err != nil {
		return nil, err
	}
	resp.ID = id

	rec := storage.Record{
		ID:          id,
		KeyPath:     n,
		SharedState: c.State,
	}

	if err := r.db.Create(rec); err != nil {
		return nil, err
	}

	return resp, nil
}

func getTxOut(bc *btcrpcclient.Client,
	txid string, vout uint32, addr string) (int64, int, string, error) {

	txhash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return 0, 0, "", err
	}

	txout, err := bc.GetTxOut(txhash, vout, false)
	if err != nil {
		return 0, 0, "", err
	}
	if txout == nil {
		return 0, 0, "", errors.New("cannot find utxo")
	}

	if txout.Coinbase {
		return 0, 0, "", errors.New("cannot use coinbase")
	}

	if len(txout.ScriptPubKey.Addresses) != 1 {
		return 0, 0, "", errors.New("wrong number of addresses")
	}
	if txout.ScriptPubKey.Addresses[0] != addr {
		return 0, 0, "", errors.New("bad address")
	}

	// yuck
	value := int64(txout.Value * 1e8)

	return value, int(txout.Confirmations), txout.BestBlock, nil
}

func getHeight(bc *btcrpcclient.Client, blockhash string) (int64, error) {
	bh, err := chainhash.NewHashFromStr(blockhash)
	if err != nil {
		return 0, err
	}
	header, err := bc.GetBlockHeaderVerbose(bh)
	if err != nil {
		return 0, err
	}
	return int64(header.Height), nil
}

func (r *Receiver) get(id string) (*channels.Receiver, error) {
	rec, err := r.db.Get(id)
	if err != nil {
		return nil, err
	}

	privKey, err := r.getKey(rec.KeyPath)
	if err != nil {
		return nil, err
	}

	c, err := channels.LoadReceiver(r.config, rec.SharedState, privKey)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (r *Receiver) Open(req models.OpenRequest) (*models.OpenResponse, error) {
	c, err := r.get(req.ID)
	if err != nil {
		return nil, err
	}
	prevState := c.State

	_, addr, err := c.State.GetFundingScript()
	if err != nil {
		return nil, err
	}

	amount, conf, blockHash, err := getTxOut(r.bc, req.TxID, req.Vout, addr)
	if err != nil {
		return nil, err
	}

	if conf < fundingMinConf {
		return nil, errors.New("too few confirmations")
	}
	if conf > softTimeout {
		return nil, errors.New("too many confirmations")
	}

	height, err := getHeight(r.bc, blockHash)
	if err != nil {
		return nil, err
	}

	resp, err := c.Open(amount, &req)
	if err != nil {
		return nil, err
	}

	newState := c.State
	newState.BlockHeight = int(height)

	if err := r.db.Update(req.ID, prevState, newState, nil); err != nil {
		return nil, err
	}

	return resp, nil
}

func (r *Receiver) validate(c *channels.Receiver, payment []byte) (bool, *models.Payment, error) {
	var p models.Payment
	if err := json.Unmarshal(payment, &p); err != nil {
		return false, nil, errors.New("invalid payment")
	}

	valid, err := c.Validate(p.Amount, payment)
	if err != nil {
		return false, nil, err
	}
	if !valid {
		return false, nil, nil
	}
	has, err := r.dir.HasTarget(p.Target)
	if err != nil {
		return false, nil, err
	}
	if !has {
		return false, nil, nil
	}

	return true, &p, nil
}

func (r *Receiver) Validate(req models.ValidateRequest) (*models.ValidateResponse, error) {
	c, err := r.get(req.ID)
	if err != nil {
		return nil, err
	}

	valid, _, err := r.validate(c, req.Payment)
	if err != nil {
		return nil, err
	}

	return &models.ValidateResponse{Valid: valid}, nil
}

func (r *Receiver) Send(req models.SendRequest) (*models.SendResponse, error) {
	c, err := r.get(req.ID)
	if err != nil {
		return nil, err
	}
	prevState := c.State

	valid, p, err := r.validate(c, req.Payment)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, errors.New("invalid payment")
	}

	resp, err := c.Send(p.Amount, &req)
	if err != nil {
		return nil, err
	}

	newState := c.State

	if err := r.db.Update(req.ID, prevState, newState, req.Payment); err != nil {
		return nil, err
	}

	return resp, nil
}

func (r *Receiver) Close(req models.CloseRequest) (*models.CloseResponse, error) {
	c, err := r.get(req.ID)
	if err != nil {
		return nil, err
	}
	prevState := c.State

	resp, err := c.Close(&req)
	if err != nil {
		return nil, err
	}

	log.Printf("closeTx: %s", hex.EncodeToString(resp.CloseTx))

	newState := c.State

	if err := r.db.Update(req.ID, prevState, newState, nil); err != nil {
		return nil, err
	}

	var tx wire.MsgTx
	err = tx.BtcDecode(bytes.NewReader(resp.CloseTx), wire.ProtocolVersion)
	if err != nil {
		return nil, err
	}

	txid, err := r.bc.SendRawTransaction(&tx, false)
	if err != nil {
		return nil, err
	}
	log.Printf("closeTx txid: %s", txid.String())

	return resp, nil
}

func (r *Receiver) Status(req models.StatusRequest) (*models.StatusResponse, error) {
	c, err := r.get(req.ID)
	if err != nil {
		return nil, err
	}

	return &models.StatusResponse{
		Status:       int(c.State.Status),
		Balance:      c.State.Balance,
		PaymentsHash: c.State.PaymentsHash[:],
	}, nil
}
