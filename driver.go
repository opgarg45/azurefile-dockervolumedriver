package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	azure "github.com/Azure/azure-sdk-for-go/storage"
	log "github.com/Sirupsen/logrus"
	"github.com/docker/go-plugins-helpers/volume"
)

type VolumeDriver struct {
	m            sync.Mutex
	cl           azure.FileServiceClient
	meta         *MetadataDriver
	accountName  string
	accountKey   string
	mountpoint   string
	removeShares bool
}

func New(accountName, accountKey, mountpoint, metadataRoot string, removeShares bool) (*VolumeDriver, error) {
	storageClient, err := azure.NewBasicClient(accountName, accountKey)
	if err != nil {
		return nil, fmt.Errorf("error creating azure client: %v", err)
	}
	metaDriver, err := NewMetadataDriver(metadataRoot)
	if err != nil {
		return nil, fmt.Errorf("cannot initialize metadata driver: %v", err)
	}
	return &VolumeDriver{
		cl:           storageClient.GetFileService(),
		meta:         metaDriver,
		accountName:  accountName,
		accountKey:   accountKey,
		mountpoint:   mountpoint,
		removeShares: removeShares,
	}, nil
}

func (v *VolumeDriver) Create(req volume.Request) (resp volume.Response) {
	v.m.Lock()
	defer v.m.Unlock()

	logctx := log.WithFields(log.Fields{
		"operation": "create",
		"name":      req.Name,
		"options":   req.Options})

	volMeta, err := v.meta.Validate(req.Options)
	if err != nil {
		resp.Err = fmt.Sprintf("error validating metadata: %v", err)
		logctx.Error(resp.Err)
		return
	}

	// Additional volume metadata
	volMeta.Account = v.accountName
	volMeta.CreatedAt = time.Now().UTC()

	share := req.Options["share"]
	if share == "" {
		resp.Err = "missing volume option: 'share'"
		logctx.Error(resp.Err)
		return
	}

	logctx.Debug("request accepted")

	// Create azure file share
	if ok, err := v.cl.CreateShareIfNotExists(share); err != nil {
		resp.Err = fmt.Sprintf("error creating azure file share: %v", err)
		logctx.Error(resp.Err)
		return
	} else if ok {
		logctx.Infof("created azure file share %q", share)
	}

	// Save volume metadata
	if err := v.meta.Set(req.Name, volMeta); err != nil {
		resp.Err = fmt.Sprintf("error saving metadata: %v", err)
		logctx.Error(resp.Err)
		return
	}
	return
}

func (v *VolumeDriver) Path(req volume.Request) (resp volume.Response) {
	v.m.Lock()
	defer v.m.Unlock()

	log.WithFields(log.Fields{
		"operation": "path", "name": req.Name,
	}).Debug("request accepted")

	resp.Mountpoint = v.pathForVolume(req.Name)
	return
}

func (v *VolumeDriver) Mount(req volume.Request) (resp volume.Response) {
	v.m.Lock()
	defer v.m.Unlock()

	logctx := log.WithFields(log.Fields{
		"operation": "mount",
		"name":      req.Name,
	})
	logctx.Debug("request accepted")

	path := v.pathForVolume(req.Name)
	if err := os.MkdirAll(path, 0700); err != nil {
		resp.Err = fmt.Sprintf("could not create mount point: %v", err)
		logctx.Error(resp.Err)
		return
	}

	meta, err := v.meta.Get(req.Name)
	if err != nil {
		resp.Err = fmt.Sprintf("could not fetch metadata: %v", err)
		logctx.Error(resp.Err)
		return
	}

	if meta.Account != v.accountName {
		resp.Err = fmt.Sprintf("volume hosted on a different account ('%s') cannot mount", meta.Account)
		logctx.Error(resp.Err)
		return
	}

	if err := mount(v.accountName, v.accountKey, meta.Options.Share, path); err != nil {
		resp.Err = err.Error()
		logctx.Error(resp.Err)
		return
	}
	resp.Mountpoint = path
	return
}

func (v *VolumeDriver) Unmount(req volume.Request) (resp volume.Response) {
	v.m.Lock()
	defer v.m.Unlock()

	logctx := log.WithFields(log.Fields{
		"operation": "unmount",
		"name":      req.Name,
	})
	logctx.Debug("request accepted")
	path := v.pathForVolume(req.Name)
	if err := unmount(path); err != nil {
		resp.Err = err.Error()
		logctx.Error(resp.Err)
		return
	}
	if err := os.RemoveAll(path); err != nil {
		resp.Err = fmt.Sprintf("error removing mountpoint: %v", err)
		logctx.Error(resp.Err)
		return
	}
	return
}

func (v *VolumeDriver) Remove(req volume.Request) (resp volume.Response) {
	v.m.Lock()
	defer v.m.Unlock()

	logctx := log.WithFields(log.Fields{
		"operation": "remove",
		"name":      req.Name,
	})
	logctx.Debug("request accepted")

	meta, err := v.meta.Get(req.Name)
	if err != nil {
		resp.Err = fmt.Sprintf("could not fetch metadata: %v", err)
		logctx.Error(resp.Err)
		return
	}

	share := meta.Options.Share
	if v.removeShares {
		if ok, err := v.cl.DeleteShareIfExists(share); err != nil {
			resp.Err = fmt.Sprintf("error removing azure file share %q: %v", share, err)
			logctx.Error(resp.Err)
			return
		} else if ok {
			logctx.Infof("removed azure file share %q", share)
		}
	} else {
		logctx.Debugf("not removing share %q upon volume removal", share)
	}
	return
}

func (v *VolumeDriver) pathForVolume(name string) string {
	return filepath.Join(v.mountpoint, name)
}

func mount(accountName, accountKey, shareName, mountpoint string) error {
	// TODO: replace with mount() syscall using docker/docker/pkg/mount
	// (currently gives hard-to-debug 'invalid argument' error with the
	// following arguments, my guess is, mount program does IP resolution
	// and essentially passes a different set of options to system call).
	cmd := exec.Command("mount", "-t", "cifs", fmt.Sprintf("//%s.file.core.windows.net/%s", accountName, shareName), mountpoint, "-o", fmt.Sprintf("vers=3.0,username=%s,password=%s,dir_mode=0777,file_mode=0777", accountName, accountKey), "--verbose")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount failed: %v\noutput=%q", err, out)
	}
	return nil
}

func unmount(mountpoint string) error {
	cmd := exec.Command("umount", mountpoint)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("unmount failed: %v\noutput=%q", err, out)
	}
	return nil
}
