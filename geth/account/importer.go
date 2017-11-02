package account

import (
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/status-im/status-go/extkeys"
)

type extendedKeyImporterBase struct{}

func (i *extendedKeyImporterBase) Import(keyStore accountKeyStorer, extKey *extkeys.ExtendedKey, password string) (address, pubKey string, err error) {
	// imports extended key, create key file (if necessary)
	account, err := keyStore.ImportExtendedKey(extKey, password)
	if err != nil {
		return "", "", err
	}
	address = account.Address.Hex()

	// obtain public key to return
	account, key, err := keyStore.AccountDecryptedKey(account, password)
	if err != nil {
		return address, "", err
	}
	pubKey = gethcommon.ToHex(crypto.FromECDSAPub(&key.PrivateKey.PublicKey))

	return
}