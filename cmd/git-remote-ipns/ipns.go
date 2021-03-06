package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"path"
	"strings"

	core "github.com/ipfs-shipyard/git-remote-ipld/core"
	ipfs "gx/ipfs/Qmab1jkxU94CuahLNFU8MxA34dA2hJ89AC681cpjMGPuN8/go-ipfs-api"

	"gx/ipfs/QmSVCWSGNwq9Lr1t4uLSMnytyJe4uL7NW7jZ3uas5BPpbX/go-git.v4/plumbing"
	"gx/ipfs/QmapdYm1b22Frv3k17fqrBYTFRxwiaVJkB299Mfn33edeB/go-cid"
)

const (
	LARGE_OBJECT_DIR    = "objects"
	LOBJ_TRACKER_PRIFIX = "//lobj"

	REFPATH_HEAD = iota
	REFPATH_REF
)

type refPath struct {
	path  string
	rType int

	hash string
}

type IpnsHandler struct {
	api *ipfs.Shell

	remoteName  string
	currentHash string

	largeObjs map[string]string

	didPush bool
}

func (h *IpnsHandler) Initialize(remote *core.Remote) error {
	h.api = ipfs.NewLocalShell()
	h.currentHash = h.remoteName
	return nil
}

func (h *IpnsHandler) Finish(remote *core.Remote) error {
	//TODO: publish
	if h.didPush {
		if err := h.fillMissingLobjs(remote.Tracker); err != nil {
			return err
		}

		remote.Logger.Printf("Pushed to IPFS as \x1b[32mipns://%s\x1b[39m\n", h.currentHash)
	}
	return nil
}

func (h *IpnsHandler) ProvideBlock(cid string, tracker *core.Tracker) ([]byte, error) {
	if h.largeObjs == nil {
		if err := h.loadObjectMap(); err != nil {
			return nil, err
		}
	}

	mappedCid, ok := h.largeObjs[cid]
	if !ok {
		return nil, core.ErrNotProvided
	}

	if err := tracker.Set(LOBJ_TRACKER_PRIFIX+"/"+cid, []byte(mappedCid)); err != nil {
		return nil, err
	}

	r, err := h.api.Cat(fmt.Sprintf("/ipfs/%s", mappedCid))
	if err != nil {
		return nil, errors.New("cat error")
	}

	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	realCid, err := h.api.DagPut(data, "raw", "git")
	if err != nil {
		return nil, err
	}

	if realCid != cid {
		return nil, fmt.Errorf("unexpected cid for provided block %s != %s", realCid, cid)
	}

	return data, nil
}

func (h *IpnsHandler) loadObjectMap() error {
	h.largeObjs = map[string]string{}

	links, err := h.api.List(h.currentHash + "/" + LARGE_OBJECT_DIR)
	if err != nil {
		//TODO: Find a better way with coreapi
		if strings.HasPrefix(err.Error(), `ls: no link named "objects" under`) {
			return nil
		}
		return err
	}

	for _, link := range links {
		h.largeObjs[link.Name] = link.Hash
	}

	return nil
}

func (h *IpnsHandler) List(remote *core.Remote, forPush bool) ([]string, error) {
	out := make([]string, 0)
	if !forPush {
		refs, err := h.paths(h.api, h.remoteName, 0)
		if err != nil {
			return nil, err
		}

		for _, ref := range refs {
			switch ref.rType {
			case REFPATH_HEAD:
				r := path.Join(strings.Split(ref.path, "/")[1:]...)
				c, err := cid.Parse(ref.hash)
				if err != nil {
					return nil, err
				}

				hash, err := core.HexFromCid(c)
				if err != nil {
					return nil, err
				}

				out = append(out, fmt.Sprintf("%s %s", hash, r))
			case REFPATH_REF:
				r := path.Join(strings.Split(ref.path, "/")[1:]...)
				dest, err := h.getRef(r)
				if err != nil {
					return nil, err
				}
				out = append(out, fmt.Sprintf("@%s %s", dest, r))
			}

		}
	} else {
		it, err := remote.Repo.Branches()
		if err != nil {
			return nil, err
		}

		err = it.ForEach(func(ref *plumbing.Reference) error {
			// resolve ref.Name().String()
			// return that

			remoteRef := make([]byte, 20)

			out = append(out, fmt.Sprintf("%s %s", hex.EncodeToString(remoteRef), ref.Name()))

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (h *IpnsHandler) Push(remote *core.Remote, local string, remoteRef string) (string, error) {
	h.didPush = true

	localRef, err := remote.Repo.Reference(plumbing.ReferenceName(local), true)
	if err != nil {
		return "", fmt.Errorf("command push: %v", err)
	}

	headHash := localRef.Hash().String()

	push := remote.NewPush()
	push.NewNode = h.bigNodePatcher(remote.Tracker)

	err = push.PushHash(headHash)
	if err != nil {
		return "", fmt.Errorf("command push: %v", err)
	}

	hash := localRef.Hash()
	remote.Tracker.Set(remoteRef, (&hash)[:])

	c, err := core.CidFromHex(headHash)
	if err != nil {
		return "", fmt.Errorf("push: %v", err)
	}

	//patch object
	h.currentHash, err = h.api.PatchLink(h.currentHash, remoteRef, c.String(), true)
	if err != nil {
		return "", fmt.Errorf("push: %v", err)
	}

	head, err := h.getRef("HEAD")
	if err != nil {
		return "", fmt.Errorf("push: %v", err)
	}
	if head == "" {
		headRef, err := h.api.Add(strings.NewReader("refs/heads/master")) //TODO: Make this smarter?
		if err != nil {
			return "", fmt.Errorf("push: %v", err)
		}

		h.currentHash, err = h.api.PatchLink(h.currentHash, "HEAD", headRef, true)
		if err != nil {
			return "", fmt.Errorf("push: %v", err)
		}
	}

	return local, nil
}

// bigNodePatcher returns a function which patches large object mapping into
// the resulting object
func (h *IpnsHandler) bigNodePatcher(tracker *core.Tracker) func(*cid.Cid, []byte) error {
	return func(hash *cid.Cid, data []byte) error {
		if len(data) > (1 << 21) {
			c, err := h.api.Add(bytes.NewReader(data))
			if err != nil {
				return err
			}

			if err := tracker.Set(LOBJ_TRACKER_PRIFIX+"/"+hash.String(), []byte(c)); err != nil {
				return err
			}

			h.currentHash, err = h.api.PatchLink(h.currentHash, "objects/"+hash.String(), c, true)
			if err != nil {
				return err
			}
		}

		return nil
	}
}

func (h *IpnsHandler) fillMissingLobjs(tracker *core.Tracker) error {
	if h.largeObjs == nil {
		if err := h.loadObjectMap(); err != nil {
			return err
		}
	}

	tracked, err := tracker.ListPrefixed(LOBJ_TRACKER_PRIFIX)
	if err != nil {
		return err
	}

	for k, v := range tracked {
		if _, has := h.largeObjs[k]; has {
			continue
		}

		k = strings.TrimPrefix(k, LOBJ_TRACKER_PRIFIX + "/")

		h.largeObjs[k] = v
		h.currentHash, err = h.api.PatchLink(h.currentHash, "objects/"+k, v, true)
		if err != nil {
			return err
		}
	}

	return nil
}

func (h *IpnsHandler) getRef(name string) (string, error) {
	r, err := h.api.Cat(path.Join(h.remoteName, name))
	if err != nil {
		if strings.Contains(err.Error(), "cat: no link named") {
			return "", nil
		}
		return "", err
	}
	defer r.Close()

	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(r)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

func (h *IpnsHandler) paths(api *ipfs.Shell, p string, level int) ([]refPath, error) {
	links, err := api.List(p)
	if err != nil {
		return nil, err
	}

	out := make([]refPath, 0)
	for _, link := range links {
		switch link.Type {
		case ipfs.TDirectory:
			if level == 0 && link.Name == LARGE_OBJECT_DIR {
				continue
			}

			sub, err := h.paths(api, path.Join(p, link.Name), level+1)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		case ipfs.TFile:
			out = append(out, refPath{path.Join(p, link.Name), REFPATH_REF, link.Hash})
		case -1: //unknown, assume git node
			out = append(out, refPath{path.Join(p, link.Name), REFPATH_HEAD, link.Hash})
		default:
			return nil, fmt.Errorf("unexpected link type %d", link.Type)
		}
	}
	return out, nil
}
