package renter

import (
	"errors"
	"io"
	"os"

	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/modules"
)

const (
	maxUploadAttempts = 5
)

// downloadPiece attempts to retrieve a file from a host.
func (r *Renter) downloadPiece(piece FilePiece, path string) error {
	return r.gateway.RPC(piece.HostIP, "RetrieveFile", func(conn modules.NetConn) (err error) {
		// Send the id of the contract for the file piece we're requesting. The
		// response will be the file piece contents.
		if err = conn.WriteObject(piece.ContractID); err != nil {
			return
		}

		// Create the file on disk.
		file, err := os.Create(path)
		if err != nil {
			return
		}
		defer file.Close()

		// Simultaneously download file and calculate its Merkle root.
		tee := io.TeeReader(
			// use a LimitedReader to ensure we don't read indefinitely
			io.LimitReader(conn, int64(piece.Contract.FileSize)),
			// each byte we read from tee will also be written to file
			file,
		)
		merkleRoot, err := crypto.ReaderMerkleRoot(tee)
		if err != nil {
			return
		}

		if merkleRoot != piece.Contract.FileMerkleRoot {
			return errors.New("host provided a file that's invalid")
		}

		return
	})
}

// threadedUploadPiece will upload the piece of a file to a randomly chosen
// host. If the wallet has insufficient balance to support uploading,
// uploadPiece will give up. The file uploading can be continued using a repair
// tool. Upon completion, the memory containg the piece's information is
// updated.
func (r *Renter) threadedUploadPiece(up modules.UploadParams, piece *FilePiece) {
	// Try 'maxUploadAttempts' hosts before giving up.
	for attempts := 0; attempts < maxUploadAttempts; attempts++ {
		// Select a host. An error here is unrecoverable.
		host, err := r.hostDB.RandomHost()
		if err != nil {
			return
		}

		// Negotiate the contract with the host. If the negotiation is
		// unsuccessful, we need to try again with a new host. Otherwise, the
		// file will be uploaded and we'll be done.
		contract, contractID, err := r.negotiateContract(host, up)
		if err != nil {
			continue
		}

		r.mu.Lock()
		*piece = FilePiece{
			HostIP:     host.IPAddress,
			Contract:   contract,
			ContractID: contractID,
			Active:     true,
		}
		r.save()
		r.mu.Unlock()
		return
	}
}

// Download downloads a file. Mutex conventions are broken to prevent doing
// network communication with io in place.
func (r *Renter) Download(nickname, filename string) error {
	// Grab the set of pieces we're downloading.
	r.mu.RLock()
	var pieces []FilePiece
	_, exists := r.files[nickname]
	if !exists {
		r.mu.RUnlock()
		return errors.New("no file of that nickname")
	}
	for _, piece := range r.files[nickname].pieces {
		if piece.Active {
			pieces = append(pieces, piece)
		}
	}
	r.mu.RUnlock()

	// We only need one piece, so iterate through the hosts until a download
	// succeeds.
	for _, piece := range pieces {
		downloadErr := r.downloadPiece(piece, filename)
		if downloadErr == nil {
			return nil
		} else {
			// log error
		}
		// r.hostDB.FlagHost(piece.Host.IPAddress)
	}

	return errors.New("Too many hosts returned errors - could not recover the file")
}

// Upload takes an upload parameters, which contain a file to upload, and then
// creates a redundant copy of the file on the Sia network.
func (r *Renter) Upload(up modules.UploadParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check for a nickname conflict.
	_, exists := r.files[up.Nickname]
	if exists {
		return errors.New("file with that nickname already exists")
	}

	// Check that the hostdb is sufficiently large to support an upload. Right
	// now that value is set to 3, but in the future the logic will be a bit
	// more complex; once there is erasure coding we'll want to hit the minimum
	// number of pieces plus some buffer before we decide that an upload is
	// okay.
	if r.hostDB.NumHosts() < 1 {
		return errors.New("not enough hosts on the network to upload a file :( - maybe you need to upgrade your software")
	}

	// Upload a piece to every host on the network.
	r.files[up.Nickname] = File{
		nickname:    up.Nickname,
		pieces:      make([]FilePiece, up.Pieces),
		startHeight: r.state.Height() + up.Duration,
		renter:      r,
	}
	for i := range r.files[up.Nickname].pieces {
		// threadedUploadPiece will change the memory that the piece points to,
		// which is useful because it means the file itself can be renamed but
		// will still point to the same underlying pieces.
		go r.threadedUploadPiece(up, &r.files[up.Nickname].pieces[i])
	}
	r.save()

	return nil
}