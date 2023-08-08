package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb"
)

func (server *Server) RaftInit() {
	conf := server.config

	raftConfig := raft.DefaultConfig()
	raftConfig.LocalID = raft.ServerID(conf.ServerID)

	var logStore raft.LogStore
	var stableStore raft.StableStore
	var snapshotStore raft.SnapshotStore

	if conf.InMemory {
		logStore = raft.NewInmemStore()
		stableStore = raft.NewInmemStore()
		snapshotStore = raft.NewInmemSnapshotStore()
	} else {
		boltdb, err := raftboltdb.NewBoltStore(filepath.Join(conf.DataDir, "raft.db"))
		if err != nil {
			log.Fatal(err)
		}

		logStore, err = raft.NewLogCache(512, boltdb)
		if err != nil {
			log.Fatal(err)
		}

		stableStore = raft.StableStore(boltdb)

		snapshotStore, err = raft.NewFileSnapshotStore(conf.DataDir, 2, os.Stdout)
		if err != nil {
			log.Fatal(err)
		}
	}

	addr := fmt.Sprintf("%s:%d", conf.BindAddr, conf.RaftBindPort)

	advertiseAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}

	raftTransport, err := raft.NewTCPTransport(
		addr,
		advertiseAddr,
		10,
		500*time.Millisecond,
		os.Stdout,
	)

	if err != nil {
		log.Fatal(err)
	}

	// Start raft server
	raftServer, err := raft.NewRaft(
		raftConfig,
		&raft.MockFSM{},
		logStore,
		stableStore,
		snapshotStore,
		raftTransport,
	)

	if err != nil {
		log.Fatalf("Could not start node with error; %s", err)
	}

	server.raft = raftServer

	if conf.JoinAddr == "" {
		// Bootstrap raft cluster
		if err := server.raft.BootstrapCluster(raft.Configuration{
			Servers: []raft.Server{
				{
					Suffrage: raft.Voter,
					ID:       raft.ServerID(conf.ServerID),
					Address:  raft.ServerAddress(addr),
				},
			},
		}).Error(); err != nil {
			log.Fatal(err)
		}
	}

}

// Implement raft.FSM interface
func (server *Server) Apply(log *raft.Log) interface{} {
	return nil
}

// Implements raft.FSM interface
func (server *Server) Snapshot() (raft.FSMSnapshot, error) {
	return nil, nil
}

// Implements raft.FSM interface
func (server *Server) Restore(snapshot io.ReadCloser) error {
	return nil
}

func (server *Server) isRaftLeader() bool {
	return server.raft.State() == raft.Leader
}

func (server *Server) addVoter(
	id raft.ServerID,
	address raft.ServerAddress,
	prevIndex uint64,
	timeout time.Duration,
) error {
	if !server.isRaftLeader() {
		return errors.New("not leader, cannot add voter")
	}
	raftConfig := server.raft.GetConfiguration()
	if err := raftConfig.Error(); err != nil {
		return errors.New("could not retrieve raft config")
	}

	// After successfully adding the voter node
	// or if voter node has already been added,
	// broadcast this success message
	msg := BroadcastMessage{
		Action: "RaftJoinSuccess",
		NodeMeta: NodeMeta{
			ServerID: id,
			RaftAddr: address,
		},
	}

	for _, s := range raftConfig.Configuration().Servers {
		// Check if a server already exists with the current attribtues
		if s.ID == id && s.Address == address {
			server.broadcastQueue.QueueBroadcast(&msg)
			return fmt.Errorf("server with id %s and address %s already exists", id, address)
		}
	}

	err := server.raft.AddVoter(id, address, prevIndex, timeout).Error()
	if err != nil {
		return err
	}

	server.broadcastQueue.QueueBroadcast(&msg)
	return nil
}

func (server *Server) removeServer(meta NodeMeta) error {
	if !server.isRaftLeader() {
		return errors.New("not leader, could not remove server")
	}

	if err := server.raft.RemoveServer(meta.ServerID, 0, 0).Error(); err != nil {
		return err
	}

	return nil
}

func (server *Server) RaftShutdown() {
	// Leadership transfer if current node is the leader
	if server.isRaftLeader() {
		err := server.raft.LeadershipTransfer().Error()

		if err != nil {
			log.Fatal(err)
		}

		fmt.Println("Leadership transfer successful.")
	}
}