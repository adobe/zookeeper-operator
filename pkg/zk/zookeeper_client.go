/**
 * Copyright (c) 2020 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */
package zk

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pravega/zookeeper-operator/api/v1beta1"
	"github.com/samuel/go-zookeeper/zk"
)

type ZookeeperClient interface {
	Connect(string) error
	CreateNode(*v1beta1.ZookeeperCluster, string) error
	NodeExists(string) (int32, error)
	UpdateNode(string, string, int32) error
	Close()
}

type zookeeperConnection interface {
	Create(string, []byte, int32, []zk.ACL) (string, error)
	Exists(string) (bool, *zk.Stat, error)
	Set(string, []byte, int32) (*zk.Stat, error)
	Close()
}

type DefaultZookeeperClient struct {
	conn zookeeperConnection
}

func (client *DefaultZookeeperClient) Connect(zkUri string) (err error) {
	host := []string{zkUri}
	conn, _, err := zk.Connect(host, time.Second*5)
	if err != nil {
		return fmt.Errorf("Failed to connect to zookeeper: %s, Reason: %v", zkUri, err)
	}
	client.conn = conn
	return nil
}

func (client *DefaultZookeeperClient) CreateNode(zoo *v1beta1.ZookeeperCluster, zNodePath string) (err error) {
	paths := strings.Split(zNodePath, "/")
	pathLength := len(paths)
	var parentPath string
	for i := 1; i < pathLength-1; i++ {
		parentPath += "/" + paths[i]
		if err := client.createNodeIfAbsent(parentPath, nil); err != nil {
			return fmt.Errorf("error creating parent zkNode: %s: %w", parentPath, err)
		}
	}
	data := "CLUSTER_SIZE=" + strconv.Itoa(int(zoo.Spec.Replicas))
	childNode := parentPath + "/" + paths[pathLength-1]
	if err := client.createNodeIfAbsent(childNode, []byte(data)); err != nil {
		return fmt.Errorf("error creating sub zkNode: %s: %w", childNode, err)
	}
	return nil
}

func (client *DefaultZookeeperClient) createNodeIfAbsent(path string, data []byte) error {
	_, err := client.conn.Create(path, data, 0, zk.WorldACL(zk.PermAll))
	if err != zk.ErrNodeExists {
		return err
	}
	exists, _, err := client.conn.Exists(path)
	if err != nil {
		return err
	}
	if !exists {
		return zk.ErrNoNode
	}
	return nil
}

func (client *DefaultZookeeperClient) UpdateNode(path string, data string, version int32) (err error) {
	if _, err := client.conn.Set(path, []byte(data), version); err != nil {
		return fmt.Errorf("Error updating zkNode: %v", err)
	}
	return nil
}

func (client *DefaultZookeeperClient) NodeExists(zNodePath string) (version int32, err error) {
	exists, zNodeStat, err := client.conn.Exists(zNodePath)
	if err != nil || !exists {
		return -1, fmt.Errorf("Znode exists check failed for path %s: %v", zNodePath, err)
	}
	return zNodeStat.Version, err
}

func (client *DefaultZookeeperClient) Close() {
	client.conn.Close()
}
