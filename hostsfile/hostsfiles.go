package hosts

import (
	"errors"
	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

//
type fileInfo struct {
	mtime time.Time
	size  int64
}

// Hostsfile represents a file containing hosts
type Hostsfiles struct {
	config    *Config
	hosts     *hostlist
	directory string
	files     map[string]*fileInfo
	hostMutex sync.RWMutex
}

func NewHostsfiles(directory string, config *Config) (*Hostsfiles, error) {
	if directory == "" {
		return nil, errors.New("no directory was pass")
	}
	h := &Hostsfiles{config: config, files: make(map[string]*fileInfo), directory: directory}

	err := h.reloadAll()
	if err != nil {
		return nil, err
	}
	if h.config.Poll > 0 {
		go h.monitorHostFiles(h.config.Poll)
	}
	return h, nil
}

func (h *Hostsfiles) reloadAll() error {
	files, err := ioutil.ReadDir(h.directory)
	if err != nil {
		return err
	}
	updateHostList := &hostlist{}
	for _, file := range files {
		var hosts *hostlist
		if hosts, err = loadHostEntries(h.directory + "/" + file.Name()); err != nil {
			return err
		}
		//Update main hostlist
		if hosts != nil {
			for _, host := range *hosts {
				updateHostList.add(host)
			}
		}
		h.files[file.Name()] = &fileInfo{size: file.Size(), mtime: file.ModTime()}
	}
	h.hosts = updateHostList
	return nil
}

func (h *Hostsfiles) FindHosts(name string) (addrs []net.IP, err error) {
	name = strings.TrimSuffix(name, ".")
	h.hostMutex.RLock()
	defer h.hostMutex.RUnlock()
	addrs = h.hosts.FindHosts(name)
	return
}

func (h *Hostsfiles) FindReverse(name string) (host string, err error) {
	h.hostMutex.RLock()
	defer h.hostMutex.RUnlock()

	for _, hostname := range *h.hosts {
		if r, _ := dns.ReverseAddr(hostname.ip.String()); name == r {
			host = dns.Fqdn(hostname.domain)
			break
		}
	}
	return
}

func loadHostEntries(path string) (*hostlist, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return newHostlist(data), nil
}

func (h *Hostsfiles) monitorHostFiles(poll int) {
	if h.directory == "" {
		return
	}

	t := time.Duration(poll) * time.Second
	for _ = range time.Tick(t) {
		files, err := ioutil.ReadDir(h.directory)
		if err != nil {
			log.Error(err)
			if os.IsNotExist(err) {
				return
			}
			continue
		}
		for _, file := range files {
			size, mtime := file.Size(), file.ModTime()
			log.Debug("checking on:", file.Name())
			if lastStat, ok := h.files[file.Name()]; ok {
				if lastStat.mtime.Equal(mtime) && lastStat.size == size {
					continue // no updates
				}
			}
			//If any of the file change, reload them all
			log.Debug("Reloaded updated hostsfile")
			h.hostMutex.Lock()
			h.reloadAll()
			h.hostMutex.Unlock()
			break
		}
	}
}
