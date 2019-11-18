package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"code.cloudfoundry.org/bytefmt"
	"github.com/nspcc-dev/neofs-proto/hash"
	"github.com/nspcc-dev/neofs-proto/object"
	"github.com/nspcc-dev/neofs-proto/query"
	"github.com/nspcc-dev/neofs-proto/refs"
	"github.com/nspcc-dev/neofs-proto/session"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

type (
	grpcState interface {
		GRPCStatus() *status.Status
	}
)

const (
	fullHeadersFlag = "full-headers"
	saltFlag        = "salt"
	verifyFlag      = "verify"
	rootFlag        = "root"
	userHeaderFlag  = "user"

	dataChunkSize = 3 * object.UnitsMB
	defaultTTL    = 2
)

var (
	objectAction = &action{
		Flags: []cli.Flag{
			hostAddr,
		},
	}
	putObjectAction = &action{
		Action: put,
		Flags: []cli.Flag{
			keyFile,
			containerID,
			filesPath,
			permissions,
			cli.BoolFlag{
				Name:  verifyFlag,
				Usage: "verify checksum after put",
			},
			cli.StringSliceFlag{
				Name:  userHeaderFlag,
				Usage: "provide optional user headers",
			},
		},
	}
	getObjectAction = &action{
		Action: get,
		Flags: []cli.Flag{
			containerID,
			objectID,
			filePath,
			permissions,
		},
	}
	delObjectAction = &action{
		Action: del,
		Flags: []cli.Flag{
			containerID,
			objectID,
			keyFile,
		},
	}
	headObjectAction = &action{
		Action: head,
		Flags: []cli.Flag{
			containerID,
			objectID,
			fullHeaders,
		},
	}
	searchObjectAction = &action{
		Action: search,
		Flags: []cli.Flag{
			containerID,
			storageGroup,
			cli.BoolFlag{
				Name:  rootFlag,
				Usage: "search only user's objects",
			},
		},
	}
	getRangeObjectAction = &action{
		Action: getRange,
		Flags: []cli.Flag{
			containerID,
			objectID,
		},
	}
	getRangeHashObjectAction = &action{
		Action: getRangeHash,
		Flags: []cli.Flag{
			containerID,
			objectID,
			cli.StringFlag{
				Name:  saltFlag,
				Usage: "salt to hash with",
			},
			cli.BoolFlag{
				Name:  verifyFlag,
				Usage: "verify hash",
			},
			filePath,
			permissions,
		},
	}
)

func del(c *cli.Context) error {
	var (
		err   error
		conn  *grpc.ClientConn
		cid   refs.CID
		objID refs.ObjectID
		key   *ecdsa.PrivateKey

		host   = c.Parent().String(hostFlag)
		cidArg = c.String(cidFlag)
		objArg = c.String(objFlag)
		keyArg = c.String(keyFlag)
	)

	if host == "" || keyArg == "" {
		return errors.Errorf("invalid input\nUsage: %s", c.Command.UsageText)
	} else if host, err = parseHostValue(host); err != nil {
		return err
	}

	// Try to receive key from file
	if key, err = parseKeyValue(keyArg); err != nil {
		return err
	}

	if cid, err = refs.CIDFromString(cidArg); err != nil {
		return errors.Wrapf(err, "can't parse CID '%s'", cidArg)
	}

	if err = objID.Parse(objArg); err != nil {
		return errors.Wrapf(err, "can't parse object id '%s'", objArg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err = grpc.DialContext(ctx, host, grpc.WithInsecure())
	if err != nil {
		return errors.Wrapf(err, "can't connect to host '%s'", host)
	}

	token, err := establishSession(ctx, conn, key, &session.Token{
		ObjectID:   []refs.ObjectID{objID},
		FirstEpoch: 0,
		LastEpoch:  math.MaxUint64,
	})
	if err != nil {
		return errors.Wrap(err, "can't establish session")
	}

	owner, err := refs.NewOwnerID(&key.PublicKey)
	if err != nil {
		return errors.Wrap(err, "could not compute owner ID")
	}

	_, err = object.NewServiceClient(conn).Delete(ctx, &object.DeleteRequest{
		Address: refs.Address{
			CID:      cid,
			ObjectID: objID,
		},
		OwnerID: owner,
		TTL:     getTTL(c),
		Token:   token,
	})
	if err != nil {
		return errors.Wrap(err, "can't perform DELETE request")
	}

	return nil
}

func head(c *cli.Context) error {
	var (
		err   error
		conn  *grpc.ClientConn
		cid   refs.CID
		objID refs.ObjectID

		host   = c.Parent().String(hostFlag)
		cidArg = c.String(cidFlag)
		objArg = c.String(objFlag)
		fh     = c.Bool(fullHeadersFlag)
	)

	if host == "" {
		return errors.Errorf("invalid input\nUsage: %s", c.Command.UsageText)
	} else if host, err = parseHostValue(host); err != nil {
		return err
	}

	if cid, err = refs.CIDFromString(cidArg); err != nil {
		return errors.Wrapf(err, "can't parse CID '%s'", cidArg)
	}

	if err = objID.Parse(objArg); err != nil {
		return errors.Wrapf(err, "can't parse object id '%s'", objArg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err = grpc.DialContext(ctx, host, grpc.WithInsecure())
	if err != nil {
		return errors.Wrapf(err, "can't connect to host '%s'", host)
	}

	resp, err := object.NewServiceClient(conn).Head(ctx, &object.HeadRequest{
		Address: refs.Address{
			CID:      cid,
			ObjectID: objID,
		},
		FullHeaders: fh,
		TTL:         getTTL(c),
	})
	if err != nil {
		return errors.Wrap(err, "can't perform HEAD request")
	}

	fmt.Println("System headers:")
	fmt.Printf("  Object ID   : %s\n", resp.Object.SystemHeader.ID)
	fmt.Printf("  Owner ID    : %s\n", resp.Object.SystemHeader.OwnerID)
	fmt.Printf("  Container ID: %s\n", resp.Object.SystemHeader.CID)
	fmt.Printf("  Payload Size: %s\n", bytefmt.ByteSize(resp.Object.SystemHeader.PayloadLength))
	fmt.Printf("  Version     : %d\n", resp.Object.SystemHeader.Version)
	fmt.Printf("  Created at  : epoch #%d, %s\n", resp.Object.SystemHeader.CreatedAt.Epoch, time.Unix(resp.Object.SystemHeader.CreatedAt.UnixTime, 0))
	if len(resp.Object.Headers) != 0 {
		fmt.Println("Other headers:")
		for i := range resp.Object.Headers {
			fmt.Println("  " + resp.Object.Headers[i].String())
		}
	}

	return nil
}

func search(c *cli.Context) error {
	var (
		err  error
		conn *grpc.ClientConn
		cid  refs.CID
		req  query.Query

		host   = c.Parent().String(hostFlag)
		cidArg = c.String(cidFlag)
		qArgs  = c.Args()
		isRoot = c.Bool(rootFlag)
		sg     = c.Bool(sgFlag)
	)

	if host == "" {
		return errors.Errorf("invalid input\nUsage: %s", c.Command.UsageText)
	} else if host, err = parseHostValue(host); err != nil {
		return err
	}

	if c.NArg()%2 != 0 {
		return errors.New("number of positional arguments must be event")
	}

	if cid, err = refs.CIDFromString(cidArg); err != nil {
		return errors.Wrapf(err, "can't parse CID '%s'", cidArg)
	}

	for i := 0; i < len(qArgs); i += 2 {
		req.Filters = append(req.Filters, query.Filter{
			Type:  query.Filter_Regex,
			Name:  qArgs[i],
			Value: qArgs[i+1],
		})
	}
	if isRoot {
		req.Filters = append(req.Filters, query.Filter{
			Type: query.Filter_Exact,
			Name: object.KeyRootObject,
		})
	}
	if sg {
		req.Filters = append(req.Filters, query.Filter{
			Type: query.Filter_Exact,
			Name: object.KeyStorageGroup,
		})
	}

	data, err := req.Marshal()
	if err != nil {
		return errors.Wrap(err, "can't marshal query")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err = grpc.DialContext(ctx, host, grpc.WithInsecure())
	if err != nil {
		return errors.Wrapf(err, "can't connect to host '%s'", host)
	}

	resp, err := object.NewServiceClient(conn).Search(ctx, &object.SearchRequest{
		ContainerID: cid,
		Query:       data,
		TTL:         getTTL(c),
		Version:     1,
	})
	if err != nil {
		return errors.Wrap(err, "can't perform SEARCH request")
	}

	fmt.Println("Container ID: Object ID")
	for i := range resp.Addresses {
		fmt.Println(resp.Addresses[i].CID.String() + ": " + resp.Addresses[i].ObjectID.String())
	}

	return nil
}

func getRange(c *cli.Context) error {
	var (
		err    error
		conn   *grpc.ClientConn
		cid    refs.CID
		objID  refs.ObjectID
		ranges []object.Range

		host   = c.Parent().String(hostFlag)
		cidArg = c.String(cidFlag)
		objArg = c.String(objFlag)
		rngArg = c.Args()
	)

	if host == "" {
		return errors.Errorf("invalid input\nUsage: %s", c.Command.UsageText)
	} else if host, err = parseHostValue(host); err != nil {
		return err
	}

	if cid, err = refs.CIDFromString(cidArg); err != nil {
		return errors.Wrapf(err, "can't parse CID '%s'", cidArg)
	}

	if err = objID.Parse(objArg); err != nil {
		return errors.Wrapf(err, "can't parse object id '%s'", objArg)
	}

	ranges, err = parseRanges(rngArg)
	if err != nil {
		return errors.Wrap(err, "can't parse ranges")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err = grpc.DialContext(ctx, host, grpc.WithInsecure())
	if err != nil {
		return errors.Wrapf(err, "can't connect to host '%s'", host)
	}

	resp, err := object.NewServiceClient(conn).GetRange(ctx, &object.GetRangeRequest{
		Address: refs.Address{
			ObjectID: objID,
			CID:      cid,
		},
		Ranges: ranges,
		TTL:    getTTL(c),
	})
	if err != nil {
		return errors.Wrap(err, "can't perform GETRANGE request")
	}

	// TODO process response
	_ = resp

	return nil
}

func getRangeHash(c *cli.Context) error {
	var (
		err    error
		conn   *grpc.ClientConn
		cid    refs.CID
		objID  refs.ObjectID
		ranges []object.Range
		salt   []byte

		host    = c.Parent().String(hostFlag)
		cidArg  = c.String(cidFlag)
		objArg  = c.String(objFlag)
		saltArg = c.String(saltFlag)
		verify  = c.Bool(verifyFlag)
		fPath   = c.String(fileFlag)
		perm    = c.Int(permFlag)
		rngArg  = c.Args()
	)

	if host == "" {
		return errors.Errorf("invalid input\nUsage: %s", c.Command.UsageText)
	} else if host, err = parseHostValue(host); err != nil {
		return err
	}

	if cid, err = refs.CIDFromString(cidArg); err != nil {
		return errors.Wrapf(err, "can't parse CID '%s'", cidArg)
	}

	if err = objID.Parse(objArg); err != nil {
		return errors.Wrapf(err, "can't parse object id '%s'", objArg)
	}

	if salt, err = hex.DecodeString(saltArg); err != nil {
		return errors.Wrapf(err, "can't decode salt")
	}

	ranges, err = parseRanges(rngArg)
	if err != nil {
		return errors.Wrap(err, "can't parse ranges")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err = grpc.DialContext(ctx, host, grpc.WithInsecure())
	if err != nil {
		return errors.Wrapf(err, "can't connect to host '%s'", host)
	}

	resp, err := object.NewServiceClient(conn).GetRangeHash(ctx, &object.GetRangeHashRequest{
		Address: refs.Address{
			ObjectID: objID,
			CID:      cid,
		},
		Ranges: ranges,
		Salt:   salt,
		TTL:    getTTL(c),
	})
	if err != nil {
		return errors.Wrap(err, "can't perform GETRANGEHASH request")
	}

	var fd *os.File
	if verify {
		if fd, err = os.OpenFile(fPath, os.O_RDONLY, os.FileMode(perm)); err != nil {
			return errors.Wrap(err, "could not open file")
		}
	}

	for i := range resp.Hashes {
		if verify {
			d := make([]byte, ranges[i].Length)
			if _, err = fd.ReadAt(d, int64(ranges[i].Offset)); err != nil && err != io.EOF {
				return errors.Wrap(err, "could not read range from file")
			}
			fmt.Print("(")
			if !hash.Sum(d[:ranges[i].Length]).Equal(resp.Hashes[i]) {
				fmt.Print("in")
			}
			fmt.Print("valid) ")
		}
		fmt.Printf("%s\n", resp.Hashes[i])
	}

	return nil
}

func parseRanges(rng []string) (ranges []object.Range, err error) {
	ranges = make([]object.Range, len(rng))
	for i := range rng {
		var (
			t   uint64
			rng = strings.Split(rng[i], ":")
		)
		if len(rng) != 2 {
			return nil, errors.New("range must have form 'offset:length'")
		}
		t, err = strconv.ParseUint(rng[0], 10, 32)
		if err != nil {
			return nil, errors.Wrap(err, "can't parse offset")
		}
		ranges[i].Offset = t

		t, err = strconv.ParseUint(rng[1], 10, 32)
		if err != nil {
			return nil, errors.Wrap(err, "can't parse length")
		}
		ranges[i].Length = t
	}
	return
}

func put(c *cli.Context) error {
	var (
		err   error
		key   *ecdsa.PrivateKey
		cid   refs.CID
		conn  *grpc.ClientConn
		fd    *os.File
		fSize int64

		keyArg = c.String(keyFlag)
		sCID   = c.String(cidFlag)
		host   = c.Parent().String(hostFlag)
		fPaths = c.StringSlice(fileFlag)
		perm   = c.Int(permFlag)
		verify = c.Bool(verifyFlag)
		userH  = c.StringSlice(userHeaderFlag)
	)

	if host == "" || keyArg == "" || sCID == "" || len(fPaths) == 0 {
		return errors.Errorf("invalid input\nUsage: %s", c.Command.UsageText)
	} else if host, err = parseHostValue(host); err != nil {
		return err
	}

	// Try to receive key from file
	if key, err = parseKeyValue(keyArg); err != nil {
		return err
	}

	if cid, err = refs.CIDFromString(sCID); err != nil {
		return errors.Wrapf(err, "can't parse CID %s", sCID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if conn, err = grpc.DialContext(ctx, host, grpc.WithInsecure()); err != nil {
		return errors.Wrapf(err, "can't connect to host '%s'", host)
	}

	owner, err := refs.NewOwnerID(&key.PublicKey)
	if err != nil {
		return errors.Wrap(err, "could not compute owner ID")
	}

	for i := range fPaths {
		fPath := fPaths[i]

		if fd, err = os.OpenFile(fPath, os.O_RDONLY, os.FileMode(perm)); err != nil {
			return errors.Wrapf(err, "can't open file %s", fPath)
		}

		fi, err := fd.Stat()
		if err != nil {
			return errors.Wrap(err, "can't get file info")
		}

		fSize = fi.Size()

		objID, err := refs.NewObjectID()
		if err != nil {
			return errors.Wrap(err, "can't generate new object ID")
		}

		token, err := establishSession(ctx, conn, key, &session.Token{
			ObjectID:   []refs.ObjectID{objID},
			FirstEpoch: 0,
			LastEpoch:  math.MaxUint64,
		})
		if st, ok := err.(grpcState); ok {
			state := st.GRPCStatus()
			return errors.Errorf("%s (%s): %s", host, state.Code(), state.Message())
		} else if err != nil {
			return errors.Wrap(err, "can't establish session")
		}

		client := object.NewServiceClient(conn)
		putClient, err := client.Put(ctx)
		if err != nil {
			return errors.Wrap(err, "put command failed on client creation")
		}

		var (
			n      int
			curOff int64
			data   = make([]byte, dataChunkSize)
			obj    = &object.Object{
				SystemHeader: object.SystemHeader{
					ID:            objID,
					OwnerID:       owner,
					CID:           cid,
					PayloadLength: uint64(fSize),
				},
				Headers: parseUserHeaders(userH),
			}
		)

		fmt.Printf("[%s] Sending header...\n", fPath)

		if err = putClient.Send(object.MakePutRequestHeader(obj, 0, getTTL(c), token)); err != nil {
			return errors.Wrap(err, "put command failed on Send object origin")
		}

		fmt.Printf("[%s] Sending data...\n", fPath)
		h := hash.Sum(nil)
		for ; err != io.EOF; curOff += int64(n) {
			if n, err = fd.ReadAt(data, curOff); err != nil && err != io.EOF {
				return errors.Wrap(err, "put command failed on file read")
			}

			if n > 0 {
				if verify {
					h, _ = hash.Concat([]hash.Hash{h, hash.Sum(data[:n])})
				}

				if err := putClient.Send(object.MakePutRequestChunk(data[:n])); err != nil && err != io.EOF {
					return errors.Wrap(err, "put command failed on Send")
				}
			}
		}

		resp, err := putClient.CloseAndRecv()
		if err != nil {
			return errors.Wrap(err, "put command failed on CloseAndRecv")
		}

		fmt.Printf("[%s] Object successfully stored\n", fPath)
		fmt.Printf("  ID: %s\n  CID: %s\n", resp.Address.ObjectID, resp.Address.CID)
		if verify {
			result := "success"
			r, err := client.GetRangeHash(ctx, &object.GetRangeHashRequest{
				Address: refs.Address{
					ObjectID: resp.Address.ObjectID,
					CID:      resp.Address.CID,
				},
				Ranges: []object.Range{{Offset: 0, Length: obj.SystemHeader.PayloadLength}},
				TTL:    getTTL(c),
			})
			if err != nil {
				result = "can't perform GETRANGEHASH request"
			} else if len(r.Hashes) == 0 {
				result = "empty hash list received"
			} else if !r.Hashes[0].Equal(h) {
				result = "hashes are not equal"
			}
			fmt.Printf("Verification result: %s.\n", result)
		}
	}

	return nil
}

func parseUserHeaders(userH []string) (headers []object.Header) {
	headers = make([]object.Header, len(userH))
	for i := range userH {
		kv := strings.SplitN(userH[i], "=", 2)
		uh := &object.UserHeader{Key: kv[0]}
		if len(kv) > 1 {
			uh.Value = kv[1]
		}
		headers[i].Value = &object.Header_UserHeader{UserHeader: uh}
	}
	return
}

func establishSession(ctx context.Context, conn *grpc.ClientConn, key *ecdsa.PrivateKey, t *session.Token) (*session.Token, error) {
	client, err := session.NewSessionClient(conn).Create(ctx)
	if err != nil {
		return nil, err
	}

	owner, err := refs.NewOwnerID(&key.PublicKey)
	if err != nil {
		return nil, errors.Wrap(err, "could not compute owner ID")
	}

	token := &session.Token{
		OwnerID:    owner,
		ObjectID:   t.ObjectID,
		FirstEpoch: t.FirstEpoch,
		LastEpoch:  t.LastEpoch,
	}

	if err := client.Send(session.NewInitRequest(token)); err != nil {
		return nil, err
	}

	resp, err := client.Recv()
	if err != nil {
		return nil, err
	}

	// receive first response and check than nothing was changed
	unsigned := resp.GetUnsigned()
	if unsigned == nil {
		return nil, errors.New("expected unsigned token")
	}

	same := unsigned.FirstEpoch == token.FirstEpoch && unsigned.LastEpoch == token.LastEpoch &&
		unsigned.OwnerID == token.OwnerID && len(unsigned.ObjectID) == len(token.ObjectID)
	if same {
		for i := range unsigned.ObjectID {
			if !unsigned.ObjectID[i].Equal(token.ObjectID[i]) {
				same = false
				break
			}
		}
	}

	if !same {
		return nil, errors.New("received token differ")
	} else if unsigned.Header.PublicKey == nil {
		return nil, errors.New("received nil public key")
	} else if err = unsigned.Sign(key); err != nil {
		return nil, errors.Wrap(err, "can't sign token")
	} else if err := client.Send(session.NewSignedRequest(unsigned)); err != nil {
		return nil, err
	} else if resp, err = client.Recv(); err != nil {
		return nil, err
	} else if result := resp.GetResult(); result != nil {
		return result, nil
	}
	return nil, errors.New("expected result token")
}

func get(c *cli.Context) error {
	var (
		err  error
		oid  refs.ObjectID
		cid  refs.CID
		conn *grpc.ClientConn
		fd   *os.File

		host  = c.Parent().String(hostFlag)
		sCID  = c.String(cidFlag)
		sOID  = c.String(objFlag)
		fPath = c.String(fileFlag)
		perm  = c.Int(permFlag)
	)

	if host == "" || sCID == "" || sOID == "" || fPath == "" {
		return errors.Errorf("invalid input\nUsage: %s", c.Command.UsageText)
	} else if host, err = parseHostValue(host); err != nil {
		return err
	}

	if cid, err = refs.CIDFromString(sCID); err != nil {
		return errors.Wrapf(err, "can't parse CID %s", sCID)
	}

	if err = oid.Parse(sOID); err != nil {
		return errors.Wrapf(err, "can't parse Object ID %s", sOID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if conn, err = grpc.DialContext(ctx, host, grpc.WithInsecure()); err != nil {
		return errors.Wrapf(err, "can't connect to host '%s'", host)
	}

	getClient, err := object.NewServiceClient(conn).Get(ctx, &object.GetRequest{
		Address: refs.Address{
			ObjectID: oid,
			CID:      cid,
		},
		TTL: getTTL(c),
	})
	if err != nil {
		return errors.Wrap(err, "get command failed on client creation")
	}

	fmt.Println("Waiting for data...")

	var objectOriginReceived bool

	for {
		resp, err := getClient.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return errors.Wrap(err, "get command received error")
		}

		if !objectOriginReceived {
			obj := resp.GetObject()

			if _, hdr := obj.LastHeader(object.HeaderType(object.TombstoneHdr)); hdr != nil {
				if err := obj.Verify(); err != nil {
					fmt.Println("Object corrupted")
					return err
				}
				fmt.Println("Object removed")
				return nil
			}

			fmt.Printf("Object origin received: %s\n", resp.GetObject().SystemHeader.ID)

			if fd, err = os.OpenFile(fPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, os.FileMode(perm)); err != nil {
				return errors.Wrapf(err, "can't open file %s", fPath)
			}

			if _, err := fd.Write(obj.Payload); err != nil && err != io.EOF {
				return errors.Wrap(err, "get command failed on file write")
			}
			objectOriginReceived = true
			fmt.Print("receiving chunks: ")
			continue
		}

		chunk := resp.GetChunk()

		fmt.Print("#")

		if _, err := fd.Write(chunk); err != nil && err != io.EOF {
			return errors.Wrap(err, "get command failed on file write")
		}
	}

	fmt.Println("\nObject successfully fetched")

	return nil
}
