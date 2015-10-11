package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"math/big"

	"github.com/btcsuite/btcd/btcec"
)

const (
	// So, 256-bit EC curve, 128-bit keys symmetric encryption, 128-bit keys for
	// HMAC, etc. Stored in bytes, here.
	securityParameter = 16

	// Default message size in bytes. This is probably too big atm?
	messageSize = 1024

	// Mix header over head. If we assume 5 hops (which seems sufficient for
	// LN, for now atleast), 32 byte group element to be re-randomized each
	// hop, and 16 byte symmetric key.
	// Overhead is: p + (2r + 2)s
	//  * p = pub key size (in bytes, for DH each hop)
	//  * r = max number of hops
	//  * s = summetric key size (in bytes)
	// It's: 32 + (2*5 + 2) * 16 = 224 bytes! But if we use secp256k1 instead of
	// Curve25519, then we've have an extra byte for the compressed keys.
	mixHeaderOverhead = 225

	// Basically an upper limit on the diameter on our node graph.
	numMaxHops = 5

	// Special destination to indicate we're at the end of the path.
	nullDestination = 0x00

	// (2r + 3)k = (2*5 + 3) * 16 = 208
	numStreamBytes = 208

	sharedSecretSize = 32

	// node_id + mac + (2*5-1)*16
	// 16 + 16 + 144
	routingInfoSize = 176
)

type LnEndpoint string

//type LnAddr btcutil.Address
type LnAddr string

type SharedSecret [sharedSecretSize]byte

var zeroNode [securityParameter]byte
var nullDest byte

// MixHeader...
type MixHeader struct {
	DHKey       *btcec.PublicKey
	RoutingInfo [routingInfoSize]byte
	HeaderMAC   [securityParameter]byte
}

// GenerateSphinxHeader...
// TODO(roasbeef): or pass in identifiers as payment path? have map from id -> pubkey
func GenerateSphinxHeader(dest []byte, identifier [securityParameter]byte,
	paymentPath []*btcec.PublicKey) (*MixHeader, [][sharedSecretSize]byte, error) {
	// Each hop performs ECDH with our ephemeral key pair to arrive at a
	// shared secret. Additionally, each hop randomizes the group element
	// for the next hop by multiplying it by the blinding factor. This way
	// we only need to transmit a single group element, and hops can't link
	// a session back to us if they have several nodes in the path.
	numHops := len(paymentPath)
	hopEphemeralPubKeys := make([]*btcec.PublicKey, numHops)
	hopSharedSecrets := make([][sha256.Size]byte, numHops)
	hopBlindingFactors := make([][sha256.Size]byte, numHops)

	// Generate a new ephemeral key to use for ECDH for this session.
	sessionKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, nil, err
	}

	// Compute the triplet for the first hop outside of the main loop.
	// Within the loop each new triplet will be computed recursively based
	// off of the blinding factor of the last hop.
	hopEphemeralPubKeys[0] = sessionKey.PubKey()
	hopSharedSecrets[0] = sha256.Sum256(btcec.GenerateSharedSecret(sessionKey, paymentPath[0]))
	hopBlindingFactors[0] = computeBlindingFactor(hopEphemeralPubKeys[0], hopSharedSecrets[0][:])

	// x * b_{0} mod n. Becomes x * b_{0} * b_{1} * ..... * b_{n} mod curve_order, etc.
	cummulativeBlind := new(big.Int).Mul(
		sessionKey.X, new(big.Int).SetBytes(hopBlindingFactors[0][:]),
	)
	cummulativeBlind.Mod(cummulativeBlind, btcec.S256().N)

	// Now recursively compute the ephemeral ECDH pub keys, the shared
	// secret, and blinding factor for each hop.
	for i := 1; i < numHops-1; i++ {
		// a_{n} = a_{n-1} x c_{n-1} -> (Y_prev_pub_key x prevBlindingFactor)
		hopEphemeralPubKeys[i] = blindGroupElement(hopEphemeralPubKeys[i-1],
			hopBlindingFactors[i-1][:])

		// s_{n} = sha256( y_{n} x c_{n-1} ) ->
		// Y_their_pub_key x (x_our_priv * all prev blinding factors mod curve_order)
		hopSharedSecrets[i] = sha256.Sum256(
			blindGroupElement(paymentPath[i], cummulativeBlind.Bytes()).X.Bytes(),
		)

		// TODO(roasbeef): prob don't need to store all blinding factors, only the prev...
		// b_{n} = sha256(a_{n} || s_{n})
		hopBlindingFactors[i] = computeBlindingFactor(hopEphemeralPubKeys[i],
			hopSharedSecrets[i][:])

		// c_{n} = c_{n-1} * b_{n} mod curve_order
		cummulativeBlind.Mul(cummulativeBlind, new(big.Int).SetBytes(hopBlindingFactors[i][:]))
		cummulativeBlind.Mod(cummulativeBlind, btcec.S256().N)
	}

	// Generate the padding, called "filler strings" in the paper.
	filler := generateHeaderPadding(numHops, hopSharedSecrets)

	// First we generate the routing info + MAC for the very last hop.
	mixHeader := make([]byte, 0, routingInfoSize)
	mixHeader = append(mixHeader, dest...)
	mixHeader = append(mixHeader, identifier[:]...)
	mixHeader = append(mixHeader,
		bytes.Repeat([]byte{0}, ((2*(numMaxHops-numHops)+2)*securityParameter-len(dest)))...)

	// Encrypt the header for the final hop with the shared secret the
	// destination will eventually derive, then pad the message out to full
	// size with the "random" filler bytes.
	streamBytes := generateRandBytes(generateKey("rho", hopSharedSecrets[numHops-1]))
	xor(mixHeader, mixHeader, streamBytes[:(2*(numMaxHops-numHops)+3)*securityParameter])
	mixHeader = append(mixHeader, filler...)

	// Calculate a MAC over the encrypted mix header for the last hop, using
	// the same shared secret key as used for encryption above.
	headerMac := calcMac(generateKey("mu", hopSharedSecrets[numHops-1]), mixHeader)

	// Now we compute the routing information for each hop, along with a
	// MAC of the routing info using the shared key for that hop.
	for i := numHops - 2; i > 0; i-- {
		// TODO(roasbeef): The node is needs to be the same length as the
		// security paramter in bytes. If we use Curve25519, then our ID's
		// are just the serialized pub keys possibly. Or, should a node's ID
		// be something P2KH style? In that case, using SHA-256 instead of
		// RIPEMD? Just serializing and truncating for now.
		nodeID := paymentPath[i+1].SerializeCompressed()[:securityParameter]

		var b bytes.Buffer
		// ID for next hop.
		b.Write(nodeID)
		// MAC for mix header.
		b.Write(headerMac[:])
		// Mix header itself.
		b.Write(mixHeader[:(2*numMaxHops-1)*securityParameter])

		streamBytes := generateRandBytes(generateKey("rho", hopSharedSecrets[i]))
		xor(mixHeader, b.Bytes(), streamBytes[:(2*numMaxHops+1)*securityParameter])
		headerMac = calcMac(generateKey("mu", hopSharedSecrets[i]), mixHeader)
	}

	var r [routingInfoSize]byte
	copy(r[:], mixHeader)
	header := &MixHeader{
		DHKey:       hopEphemeralPubKeys[0],
		RoutingInfo: r,
		HeaderMAC:   headerMac,
	}

	return header, hopSharedSecrets, nil
}

// generateHeaderPadding...
// At each step, we add 2*securityParameter padding of zeroes, concatenate it
// to the previous filler, then decrypt it (XOR) with the secret key of the
// current hop. When encrypting the mix header we essentially do the reverse of
// this operation: we "encrypt" the padding, and drop 2*k number of zeroes. As
// nodes process the mix header they add the padding (2*k) and then decrypt
// eventually leaving only the original "filler" bytes produced by this function
// at the last hop. Using this methodology, the size of the mix header stays
// constant at each hop.
func generateHeaderPadding(numHops int, sharedSecrets [][sharedSecretSize]byte) []byte {
	var filler []byte
	for i := 1; i < numHops; i++ {
		slice := (2*(numMaxHops-1) + 3) * securityParameter
		padding := bytes.Repeat([]byte{0}, 2*securityParameter)

		var tempBuf bytes.Buffer
		tempBuf.Write(filler)
		tempBuf.Write(padding)

		streamBytes := generateRandBytes(generateKey("rho", sharedSecrets[i-1]))

		xor(filler, tempBuf.Bytes(), streamBytes[slice:])
	}

	return filler
}

// CreateForwardingMessage...
// TODO(roasbeef): Identifier should also been the same size as the security paramter.
// Should probably go with k = 32 then.
func CreateForwardingMessage(route []*btcec.PublicKey, dest LnAddr,
	identifier [securityParameter]byte, message []byte) (*MixHeader, *[messageSize]byte, error) {
	routeLength := len(route)

	// Compute the mix header, and shared secerts for each hop.
	mixHeader, secrets, err := GenerateSphinxHeader([]byte{nullDest}, zeroNode, route)
	if err != nil {
		return nil, nil, err
	}

	// Now for the body of the message. The next-node ID is set to all
	// zeroes in order to notify the final op that the message is meant for
	// them. m = 0^k || dest || msg || padding.
	var body [messageSize]byte
	n := copy(body[:], bytes.Repeat([]byte{0}, securityParameter))
	// TODO(roasbeef): destination vs identifier (node id) format.
	n += copy(body[n:], []byte(dest))
	n += copy(body[n:], message)
	// TODO(roasbeef): make pad and unpad functions.
	n += copy(body[n:], []byte{0x7f})
	n += copy(body[n:], bytes.Repeat([]byte{0xff}, messageSize-len(body)))

	// Now we construct the onion. Walking backwards from the last hop, we
	// encrypt the message with the shared secret for each hop in the path.
	onion := lionessEncode(generateKey("pi", secrets[routeLength-1]), body)
	for i := routeLength - 2; i > 0; i-- {
		onion = lionessEncode(generateKey("pi", secrets[i]), onion)
	}

	return mixHeader, &onion, nil
}

// lionEncode...
// block cipher with a block size equivalent to our message size
// http://www.cl.cam.ac.uk/~rja14/Papers/bear-lion.pdf (section 6)
func lionessEncode(key [securityParameter]byte, message [messageSize]byte) [messageSize]byte {
	//var l [securityParameter]byte
	//var r [messageSize + securityParameter]byte

	sha := sha256.New()
	var cipherText [messageSize]byte

	// Round 1.
	sha.Write(message[securityParameter:])
	sha.Write(key[:])
	sha.Write([]byte{1})
	xor(cipherText[:], sha.Sum(nil)[:securityParameter], message[:securityParameter])
	copy(cipherText[securityParameter:], message[securityParameter:])

	// Round 2.
	var k2 [securityParameter]byte
	xor(k2[:], cipherText[:securityParameter], key[:])
	block, _ := aes.NewCipher(k2[:])
	stream := cipher.NewCTR(block, bytes.Repeat([]byte{0}, aes.BlockSize))
	stream.XORKeyStream(cipherText[securityParameter:], cipherText[securityParameter:])

	sha.Reset()

	// Round 3.
	sha.Write(cipherText[securityParameter:])
	sha.Write(key[:])
	sha.Write([]byte{3})
	xor(cipherText[:], sha.Sum(nil)[:securityParameter], cipherText[:securityParameter])

	// Round 4.
	var k4 [securityParameter]byte
	xor(k4[:], cipherText[:securityParameter], key[:])
	block, _ = aes.NewCipher(k4[:])
	stream = cipher.NewCTR(block, bytes.Repeat([]byte{0}, aes.BlockSize))
	stream.XORKeyStream(cipherText[securityParameter:], cipherText[securityParameter:])

	return cipherText
}

// lionDecode...
func lionessDecode() {
}

// calcMac....
func calcMac(key [securityParameter]byte, msg []byte) [securityParameter]byte {
	hmac := hmac.New(sha256.New, key[:])
	hmac.Write(msg)
	h := hmac.Sum(nil)

	var mac [securityParameter]byte
	copy(mac[:], h[:securityParameter])

	return mac
}

// xor...
func xor(dst, a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		dst[i] = a[i] ^ b[i]
	}
	return n
}

// generateKey...
// used to key rand padding generation, mac, and lionness
func generateKey(keyType string, sharedKey [sharedSecretSize]byte) [securityParameter]byte {
	mac := hmac.New(sha256.New, []byte(keyType))
	mac.Write(sharedKey[:])
	h := mac.Sum(nil)

	var key [securityParameter]byte
	copy(key[:], h[:securityParameter])

	return key
}

// generateRandBytes...
// generates
func generateRandBytes(key [securityParameter]byte) [numStreamBytes]byte {
	var r [numStreamBytes]byte

	block, _ := aes.NewCipher(key[:])

	// We use AES in CTR mode to generate a psuedo randmom stream of bytes
	// by encrypting a plaintext of all zeroes.
	randBytes := make([]byte, numStreamBytes)
	plainText := bytes.Repeat([]byte{0}, numStreamBytes)

	// Our IV is just zero....
	iv := bytes.Repeat([]byte{0}, aes.BlockSize)

	stream := cipher.NewCTR(block, iv)
	stream.XORKeyStream(randBytes, plainText)

	copy(r[:], randBytes)

	return r
}

// ComputeBlindingFactor for the next hop given the ephemeral pubKey and
// sharedSecret for this hop. The blinding factor is computed as the
// sha-256(pubkey || sharedSecret).
func computeBlindingFactor(hopPubKey *btcec.PublicKey, hopSharedSecret []byte) [sha256.Size]byte {
	sha := sha256.New()
	sha.Write(hopPubKey.SerializeCompressed())
	sha.Write(hopSharedSecret)

	var hash [sha256.Size]byte
	copy(hash[:], sha.Sum(nil))
	return hash
}

// blindGroupElement blinds the group element by performing scalar
// multiplication of the group element by blindingFactor: G x blindingFactor.
func blindGroupElement(hopPubKey *btcec.PublicKey, blindingFactor []byte) *btcec.PublicKey {
	newX, newY := hopPubKey.Curve.ScalarMult(hopPubKey.X, hopPubKey.Y, blindingFactor[:])
	return &btcec.PublicKey{hopPubKey.Curve, newX, newY}
}

// SphinxPayload...
type SphinxPayload struct {
}

// SphinxPacket...
type SphinxPacket struct {
}