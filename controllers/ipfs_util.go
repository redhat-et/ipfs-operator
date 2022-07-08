package controllers

import (
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	mrand "math/rand"

	ci "github.com/libp2p/go-libp2p-core/crypto"
	peer "github.com/libp2p/go-libp2p-core/peer"
)

func init() {
	seed := make([]byte, 8)
	_, err := crand.Read(seed)
	if err != nil {
		panic(err)
	}

	useed, _ := binary.Uvarint(seed)
	mrand.Seed(int64(useed))
}

func newClusterSecret() (string, error) {
	buf := make([]byte, 32)
	_, err := mrand.Read(buf)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func newKey() (ci.PrivKey, peer.ID, error) {
	priv, pub, err := ci.GenerateKeyPair(ci.Ed25519, 4096)
	if err != nil {
		return nil, "", err
	}
	peerid, err := peer.IDFromPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	return priv, peerid, nil
}
