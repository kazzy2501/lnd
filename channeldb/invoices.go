package channeldb

import (
	"bytes"
	"fmt"
	"io"
	"time"

	"github.com/boltdb/bolt"
	"github.com/btcsuite/fastsha256"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	// invoiceBucket is the name of the bucket within the database that
	// stores all data related to invoices no matter their final state.
	// Within the invoice bucket, each invoice is keyed by its invoice ID
	// which is a monotonically increasing uint32.
	invoiceBucket = []byte("invoices")

	// paymentHashIndexBucket is the name of the sub-bucket within the
	// invoiceBucket which indexes all invoices by their payment hash. The
	// payment hash is the sha256 of the invoice's payment preimage. This
	// index is used to detect duplicates, and also to provide a fast path
	// for looking up incoming HTLC's to determine if we're able to settle
	// them fully.
	invoiceIndexBucket = []byte("paymenthashes")

	// numInvoicesKey is the name of key which houses the auto-incrementing
	// invoice ID which is essentially used as a primary key. With each
	// invoice inserted, the primary key is incremented by one. This key is
	// stored within the invoiceIndexBucket. Within the invoiceBucket
	// invoices are uniquely identified by the invoice ID.
	numInvoicesKey = []byte("nik")
)

const (
	// MaxMemoSize is maximum size of the memo field within invoices stored
	// in the database.
	MaxMemoSize = 1024

	// MaxReceiptSize is the maximum size of the payment receipt stored
	// within the database along side incoming/outgoing invoices.
	MaxReceiptSize = 1024
)

// ContractTerm is a companion struct to the Invoice struct. This struct houses
// the necessary conditions required before the invoice can be considered fully
// settled by the payee.
type ContractTerm struct {
	// PaymentPreimage is the preimage which is to be revealed in the
	// occasion that an HTLC paying to the hash of this preimage is
	// extended.
	PaymentPreimage [32]byte

	// Value is the expected amount to be payed to an HTLC which can be
	// satisfied by the above preimage.
	Value btcutil.Amount

	// Settled indicates if this particular contract term has been fully
	// settled by the payer.
	Settled bool
}

// Invoice is a payment invoice generated by a payee in order to request
// payment for some good or service. The inclusion of invoices within Lightning
// creates a payment work flow for merchants very similar to that of the
// existing financial system within PayPal, etc.  Invoices are added to the
// database when a payment is requested, then can be settled manually once the
// payment is received at the upper layer. For record keeping purposes,
// invoices are never deleted from the database, instead a bit is toggled
// denoting the invoice has been fully settled. Within the database, all
// invoices must have a unique payment hash which is generated by taking the
// sha256 of the payment
// preimage.
type Invoice struct {
	// Memo is an optional memo to be stored along side an invoice.  The
	// memo may contain further details pertaining to the invoice itself,
	// or any other message which fits within the size constraints.
	Memo []byte

	// Receipt is an optional field dedicated for storing a
	// cryptographically binding receipt of payment.
	//
	// TODO(roasbeef): document scheme.
	Receipt []byte

	// CreationDate is the exact time the invoice was created.
	CreationDate time.Time

	// Terms are the contractual payment terms of the invoice. Once
	// all the terms have been satisfied by the payer, then the invoice can
	// be considered fully fulfilled.
	//
	// TODO(roasbeef): later allow for multiple terms to fulfill the final
	// invoice: payment fragmentation, etc.
	Terms ContractTerm
}

func validateInvoice(i *Invoice) error {
	if len(i.Memo) > MaxMemoSize {
		return fmt.Errorf("max length a memo is %v, and invoice "+
			"of length %v was provided", MaxMemoSize, len(i.Memo))
	}
	if len(i.Receipt) > MaxReceiptSize {
		return fmt.Errorf("max length a receipt is %v, and invoice "+
			"of length %v was provided", MaxReceiptSize,
			len(i.Receipt))
	}
	return nil
}

// AddInvoice inserts the targeted invoice into the database. If the invoice
// has *any* payment hashes which already exists within the database, then the
// insertion will be aborted and rejected due to the strict policy banning any
// duplicate payment hashes.
func (d *DB) AddInvoice(i *Invoice) error {
	if err := validateInvoice(i); err != nil {
		return err
	}
	return d.Update(func(tx *bolt.Tx) error {
		invoices, err := tx.CreateBucketIfNotExists(invoiceBucket)
		if err != nil {
			return err
		}

		invoiceIndex, err := invoices.CreateBucketIfNotExists(invoiceIndexBucket)
		if err != nil {
			return err
		}

		// Ensure that an invoice an identical payment hash doesn't
		// already exist within the index.
		paymentHash := fastsha256.Sum256(i.Terms.PaymentPreimage[:])
		if invoiceIndex.Get(paymentHash[:]) != nil {
			return ErrDuplicateInvoice
		}

		// If the current running payment ID counter hasn't yet been
		// created, then create it now.
		var invoiceNum uint32
		invoiceCounter := invoiceIndex.Get(numInvoicesKey)
		if invoiceCounter == nil {
			var scratch [4]byte
			byteOrder.PutUint32(scratch[:], invoiceNum)
			if err := invoiceIndex.Put(numInvoicesKey, scratch[:]); err != nil {
				return nil
			}
		} else {
			invoiceNum = byteOrder.Uint32(invoiceCounter)
		}

		return putInvoice(invoices, invoiceIndex, i, invoiceNum)
	})
}

// LookupInvoice attempts to look up an invoice according to it's 32 byte
// payment hash. In an invoice which can settle the HTLC identified by the
// passed payment hash isn't found, then an error is returned. Otherwise, the
// full invoice is returned. Before setting the incoming HTLC, the values
// SHOULD be checked to ensure the payer meets the agreed upon contractual
// terms of the payment.
func (d *DB) LookupInvoice(paymentHash [32]byte) (*Invoice, error) {
	var invoice *Invoice
	err := d.View(func(tx *bolt.Tx) error {
		invoices := tx.Bucket(invoiceBucket)
		if invoices == nil {
			return ErrInvoiceNotFound
		}
		invoiceIndex := invoices.Bucket(invoiceIndexBucket)
		if invoiceIndex == nil {
			return ErrInvoiceNotFound
		}

		// Check the invoice index to see if an invoice paying to this
		// hash exists within the DB.
		invoiceNum := invoiceIndex.Get(paymentHash[:])
		if invoiceNum == nil {
			return ErrInvoiceNotFound
		}

		// An invoice matching the payment hash has been found, so
		// retrieve the record of the invoice itself.
		i, err := fetchInvoice(invoiceNum, invoices)
		if err != nil {
			return err
		}
		invoice = i

		return nil
	})
	if err != nil {
		return nil, err
	}

	return invoice, nil
}

// FetchAllInvoices returns all invoices currently stored within the database.
// If the pendingOnly param is true, then only unsettled invoices will be
// returned, skipping all invoices that are fully settled.
func (d *DB) FetchAllInvoices(pendingOnly bool) ([]*Invoice, error) {
	var invoices []*Invoice

	err := d.View(func(tx *bolt.Tx) error {
		invoiceB := tx.Bucket(invoiceBucket)
		if invoiceB == nil {
			return ErrNoInvoicesCreated
		}

		// Iterate through the entire key space of the top-level
		// invoice bucket. If key with a non-nil value stores the next
		// invoice ID which maps to the corresponding invoice.
		return invoiceB.ForEach(func(k, v []byte) error {
			if v == nil {
				return nil
			}

			invoiceReader := bytes.NewReader(v)
			invoice, err := deserializeInvoice(invoiceReader)
			if err != nil {
				return err
			}

			if pendingOnly && invoice.Terms.Settled {
				return nil
			}

			invoices = append(invoices, invoice)

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return invoices, nil
}

// SettleInvoice attempts to mark an invoice corresponding to the passed
// payment hash as fully settled. If an invoice matching the passed payment
// hash doesn't existing within the database, then the action will fail with a
// "not found" error.
func (d *DB) SettleInvoice(paymentHash [32]byte) error {
	return d.Update(func(tx *bolt.Tx) error {
		invoices, err := tx.CreateBucketIfNotExists(invoiceBucket)
		if err != nil {
			return err
		}
		invoiceIndex, err := invoices.CreateBucketIfNotExists(invoiceIndexBucket)
		if err != nil {
			return err
		}

		// Check the invoice index to see if an invoice paying to this
		// hash exists within the DB.
		invoiceNum := invoiceIndex.Get(paymentHash[:])
		if invoiceNum == nil {
			return ErrInvoiceNotFound
		}

		return settleInvoice(invoices, invoiceNum)
	})
}

func putInvoice(invoices *bolt.Bucket, invoiceIndex *bolt.Bucket,
	i *Invoice, invoiceNum uint32) error {

	// Create the invoice key which is just the big-endian representation
	// of the invoice number.
	var invoiceKey [4]byte
	byteOrder.PutUint32(invoiceKey[:], invoiceNum)

	// Increment the num invoice counter index so the next invoice bares
	// the proper ID.
	var scratch [4]byte
	invoiceCounter := invoiceNum + 1
	byteOrder.PutUint32(scratch[:], invoiceCounter)
	if err := invoiceIndex.Put(numInvoicesKey, scratch[:]); err != nil {
		return err
	}

	// Add the payment hash to the invoice index. This'll let us quickly
	// identify if we can settle an incoming payment, and also to possibly
	// allow a single invoice to have multiple payment installations.
	paymentHash := fastsha256.Sum256(i.Terms.PaymentPreimage[:])
	if err := invoiceIndex.Put(paymentHash[:], invoiceKey[:]); err != nil {
		return err
	}

	// Finally, serialize the invoice itself to be written to the disk.
	var buf bytes.Buffer
	if err := serializeInvoice(&buf, i); err != nil {
		return nil
	}

	return invoices.Put(invoiceKey[:], buf.Bytes())
}

func serializeInvoice(w io.Writer, i *Invoice) error {
	if err := wire.WriteVarBytes(w, 0, i.Memo[:]); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(w, 0, i.Receipt[:]); err != nil {
		return err
	}

	birthBytes, err := i.CreationDate.MarshalBinary()
	if err != nil {
		return err
	}
	if err := wire.WriteVarBytes(w, 0, birthBytes); err != nil {
		return err
	}

	if _, err := w.Write(i.Terms.PaymentPreimage[:]); err != nil {
		return err
	}

	var scratch [8]byte
	byteOrder.PutUint64(scratch[:], uint64(i.Terms.Value))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	var settleByte [1]byte
	if i.Terms.Settled {
		settleByte[0] = 1
	}
	if _, err := w.Write(settleByte[:]); err != nil {
		return err
	}

	return nil
}

func fetchInvoice(invoiceNum []byte, invoices *bolt.Bucket) (*Invoice, error) {
	invoiceBytes := invoices.Get(invoiceNum)
	if invoiceBytes == nil {
		return nil, ErrInvoiceNotFound
	}

	invoiceReader := bytes.NewReader(invoiceBytes)

	return deserializeInvoice(invoiceReader)
}

func deserializeInvoice(r io.Reader) (*Invoice, error) {
	var err error
	invoice := &Invoice{}

	// TODO(roasbeef): use read full everywhere
	invoice.Memo, err = wire.ReadVarBytes(r, 0, MaxMemoSize, "")
	if err != nil {
		return nil, err
	}
	invoice.Receipt, err = wire.ReadVarBytes(r, 0, MaxReceiptSize, "")
	if err != nil {
		return nil, err
	}

	birthBytes, err := wire.ReadVarBytes(r, 0, 300, "birth")
	if err != nil {
		return nil, err
	}
	if err := invoice.CreationDate.UnmarshalBinary(birthBytes); err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, invoice.Terms.PaymentPreimage[:]); err != nil {
		return nil, err
	}
	var scratch [8]byte
	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return nil, err
	}
	invoice.Terms.Value = btcutil.Amount(byteOrder.Uint64(scratch[:]))

	var settleByte [1]byte
	if _, err := io.ReadFull(r, settleByte[:]); err != nil {
		return nil, err
	}
	if settleByte[0] == 1 {
		invoice.Terms.Settled = true
	}

	return invoice, nil
}

func settleInvoice(invoices *bolt.Bucket, invoiceNum []byte) error {
	invoice, err := fetchInvoice(invoiceNum, invoices)
	if err != nil {
		return err
	}

	invoice.Terms.Settled = true

	var buf bytes.Buffer
	if err := serializeInvoice(&buf, invoice); err != nil {
		return nil
	}

	return invoices.Put(invoiceNum[:], buf.Bytes())
}
