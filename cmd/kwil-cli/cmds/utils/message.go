package utils

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/kwilteam/kwil-db/cmd/kwil-cli/config"
	"github.com/kwilteam/kwil-db/pkg/abci"
	"github.com/kwilteam/kwil-db/pkg/client/types"
	"github.com/kwilteam/kwil-db/pkg/transactions"
)

// respStr represents a string in cli
type respStr string

func (s respStr) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Message string `json:"message"`
	}{
		Message: string(s),
	})
}

func (s respStr) MarshalText() ([]byte, error) {
	return []byte(s), nil
}

// respSig represents a signature in cli
type respSig []byte

func (r respSig) Hex() string {
	return hex.EncodeToString(r)
}

func (r respSig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Bytes string `json:"signature"` // HEX
	}{
		Bytes: r.Hex(),
	})
}

func (r respSig) MarshalText() ([]byte, error) {
	return []byte(fmt.Sprintf("Signature: %s\n", r.Hex())), nil
}

// respTxQuery is used to represent a transaction response in cli
type respTxQuery struct {
	Msg *types.TcTxQueryResponse
}

func (r *respTxQuery) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Hash     string                         `json:"hash"` // HEX
		Height   int64                          `json:"height"`
		Tx       transactions.Transaction       `json:"tx"`
		TxResult transactions.TransactionResult `json:"tx_result"`
	}{
		Hash:     hex.EncodeToString(r.Msg.Hash),
		Height:   r.Msg.Height,
		Tx:       r.Msg.Tx,
		TxResult: r.Msg.TxResult,
	})
}

func (r *respTxQuery) MarshalText() ([]byte, error) {
	status := "failed"
	if r.Msg.Height == -1 {
		status = "pending"
	} else if r.Msg.TxResult.Code == abci.CodeOk.Uint32() {
		status = "success"
	}

	msg := fmt.Sprintf(`Transaction ID: %s
Status: %s
Height: %d
Log: %s
`,
		hex.EncodeToString(r.Msg.Hash),
		status,
		r.Msg.Height,
		r.Msg.TxResult.Log,
	)

	return []byte(msg), nil
}

// respGenWalletInfo is used to represent a generated wallet info in cli
type respGenWalletInfo struct {
	info *generatedWalletInfo
}

// generatedWalletInfo is used to represent a generated wallet info
type generatedWalletInfo struct {
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
	Address    string `json:"address"`
}

func (r *respGenWalletInfo) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.info)
}

func (r *respGenWalletInfo) MarshalText() ([]byte, error) {
	printKeyDesc := `PrivateKey: %s
PublicKey: 	%s
Address: 	%s
`
	msg := fmt.Sprintf(printKeyDesc, r.info.PrivateKey, r.info.PublicKey, r.info.Address)
	return []byte(msg), nil
}

// respKwilCliConfig is used to represent a kwil-cli config in cli
type respKwilCliConfig struct {
	cfg *config.KwilCliConfig
}

func (r *respKwilCliConfig) MarshalJSON() ([]byte, error) {
	cfg := r.cfg.ToPersistedConfig()
	cfg.PrivateKey = "***"
	return json.Marshal(cfg)
}

func (r *respKwilCliConfig) MarshalText() ([]byte, error) {
	var msg bytes.Buffer
	cfg := r.cfg.ToPersistedConfig()
	cfg.PrivateKey = "***"

	v := reflect.ValueOf(cfg)
	t := v.Type()

	if t.Kind() == reflect.Ptr {
		v = v.Elem()
		t = t.Elem()
	}

	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		fieldValue := v.Field(i)
		msg.WriteString(fmt.Sprintf("%s: %v\n", field.Name, fieldValue))
	}

	return msg.Bytes(), nil
}