/*
 * Minimalist Object Storage, (C) 2015 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package donut

import (
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"io/ioutil"

	"github.com/minio/minio/pkg/iodine"
	"github.com/minio/minio/pkg/storage/donut"
	"github.com/minio/minio/pkg/storage/drivers"
	"github.com/minio/minio/pkg/utils/log"
)

// donutDriver - creates a new single disk drivers driver using donut
type donutDriver struct {
	donut donut.Donut
	paths []string
	lock  *sync.RWMutex
}

// This is a dummy nodeDiskMap which is going to be deprecated soon
// once the Management API is standardized, this map is useful for now
// to show multi disk API correctness and parity calculation
//
// Ideally this should be obtained from per node configuration file
func createNodeDiskMap(p string) map[string][]string {
	nodes := make(map[string][]string)
	nodes["localhost"] = make([]string, 16)
	for i := 0; i < len(nodes["localhost"]); i++ {
		diskPath := filepath.Join(p, strconv.Itoa(i))
		if _, err := os.Stat(diskPath); err != nil {
			if os.IsNotExist(err) {
				os.MkdirAll(diskPath, 0700)
			}
		}
		nodes["localhost"][i] = diskPath
	}
	return nodes
}

// This is a dummy nodeDiskMap which is going to be deprecated soon
// once the Management API is standardized, and we have way of adding
// and removing disks. This is useful for now to take inputs from CLI
func createNodeDiskMapFromSlice(paths []string) map[string][]string {
	diskPaths := make([]string, len(paths))
	nodes := make(map[string][]string)
	for i, p := range paths {
		diskPath := filepath.Join(p, strconv.Itoa(i))
		if _, err := os.Stat(diskPath); err != nil {
			if os.IsNotExist(err) {
				os.MkdirAll(diskPath, 0700)
			}
		}
		diskPaths[i] = diskPath
	}
	nodes["localhost"] = diskPaths
	return nodes
}

// Start a single disk subsystem
func Start(paths []string) (chan<- string, <-chan error, drivers.Driver) {
	ctrlChannel := make(chan string)
	errorChannel := make(chan error)

	// Soon to be user configurable, when Management API is available
	// we should remove "default" to something which is passed down
	// from configuration paramters
	var d donut.Donut
	var err error
	if len(paths) == 1 {
		d, err = donut.NewDonut("default", createNodeDiskMap(paths[0]))
		if err != nil {
			err = iodine.New(err, nil)
			log.Error.Println(err)
		}
	} else {
		d, err = donut.NewDonut("default", createNodeDiskMapFromSlice(paths))
		if err != nil {
			err = iodine.New(err, nil)
			log.Error.Println(err)
		}
	}
	s := new(donutDriver)
	s.donut = d
	s.paths = paths
	s.lock = new(sync.RWMutex)

	go start(ctrlChannel, errorChannel, s)
	return ctrlChannel, errorChannel, s
}

func start(ctrlChannel <-chan string, errorChannel chan<- error, s *donutDriver) {
	close(errorChannel)
}

// byBucketName is a type for sorting bucket metadata by bucket name
type byBucketName []drivers.BucketMetadata

func (b byBucketName) Len() int           { return len(b) }
func (b byBucketName) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byBucketName) Less(i, j int) bool { return b[i].Name < b[j].Name }

// ListBuckets returns a list of buckets
func (d donutDriver) ListBuckets() (results []drivers.BucketMetadata, err error) {
	d.lock.RLock()
	defer d.lock.RUnlock()
	if d.donut == nil {
		return nil, iodine.New(drivers.InternalError{}, nil)
	}
	buckets, err := d.donut.ListBuckets()
	if err != nil {
		return nil, err
	}
	for name, metadata := range buckets {
		result := drivers.BucketMetadata{
			Name:    name,
			Created: metadata.Created,
		}
		results = append(results, result)
	}
	sort.Sort(byBucketName(results))
	return results, nil
}

// CreateBucket creates a new bucket
func (d donutDriver) CreateBucket(bucketName, acl string) error {
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.donut == nil {
		return iodine.New(drivers.InternalError{}, nil)
	}
	if !drivers.IsValidBucketACL(acl) {
		return iodine.New(drivers.InvalidACL{ACL: acl}, nil)
	}
	if drivers.IsValidBucket(bucketName) && !strings.Contains(bucketName, ".") {
		if strings.TrimSpace(acl) == "" {
			acl = "private"
		}
		if err := d.donut.MakeBucket(bucketName, acl); err != nil {
			switch iodine.ToError(err).(type) {
			case donut.BucketExists:
				return iodine.New(drivers.BucketExists{Bucket: bucketName}, nil)
			}
			return iodine.New(err, nil)
		}
		return nil
	}
	return iodine.New(drivers.BucketNameInvalid{Bucket: bucketName}, nil)
}

// GetBucketMetadata retrieves an bucket's metadata
func (d donutDriver) GetBucketMetadata(bucketName string) (drivers.BucketMetadata, error) {
	d.lock.RLock()
	defer d.lock.RUnlock()
	if d.donut == nil {
		return drivers.BucketMetadata{}, iodine.New(drivers.InternalError{}, nil)
	}
	if !drivers.IsValidBucket(bucketName) || strings.Contains(bucketName, ".") {
		return drivers.BucketMetadata{}, drivers.BucketNameInvalid{Bucket: bucketName}
	}
	metadata, err := d.donut.GetBucketMetadata(bucketName)
	if err != nil {
		return drivers.BucketMetadata{}, iodine.New(drivers.BucketNotFound{Bucket: bucketName}, nil)
	}
	bucketMetadata := drivers.BucketMetadata{
		Name:    bucketName,
		Created: metadata.Created,
		ACL:     drivers.BucketACL(metadata.ACL),
	}
	return bucketMetadata, nil
}

// SetBucketMetadata sets bucket's metadata
func (d donutDriver) SetBucketMetadata(bucketName, acl string) error {
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.donut == nil {
		return iodine.New(drivers.InternalError{}, nil)
	}
	if !drivers.IsValidBucket(bucketName) || strings.Contains(bucketName, ".") {
		return iodine.New(drivers.BucketNameInvalid{Bucket: bucketName}, nil)
	}
	if strings.TrimSpace(acl) == "" {
		acl = "private"
	}
	bucketMetadata := make(map[string]string)
	bucketMetadata["acl"] = acl
	err := d.donut.SetBucketMetadata(bucketName, bucketMetadata)
	if err != nil {
		return iodine.New(drivers.BucketNotFound{Bucket: bucketName}, nil)
	}
	return nil
}

// GetObject retrieves an object and writes it to a writer
func (d donutDriver) GetObject(target io.Writer, bucketName, objectName string) (int64, error) {
	d.lock.RLock()
	defer d.lock.RUnlock()
	if d.donut == nil {
		return 0, iodine.New(drivers.InternalError{}, nil)
	}
	if !drivers.IsValidBucket(bucketName) || strings.Contains(bucketName, ".") {
		return 0, iodine.New(drivers.BucketNameInvalid{Bucket: bucketName}, nil)
	}
	if !drivers.IsValidObjectName(objectName) || strings.TrimSpace(objectName) == "" {
		return 0, iodine.New(drivers.ObjectNameInvalid{Object: objectName}, nil)
	}
	reader, size, err := d.donut.GetObject(bucketName, objectName)
	if err != nil {
		return 0, iodine.New(drivers.ObjectNotFound{
			Bucket: bucketName,
			Object: objectName,
		}, nil)
	}
	n, err := io.CopyN(target, reader, size)
	return n, iodine.New(err, nil)
}

// GetPartialObject retrieves an object range and writes it to a writer
func (d donutDriver) GetPartialObject(w io.Writer, bucketName, objectName string, start, length int64) (int64, error) {
	d.lock.RLock()
	defer d.lock.RUnlock()
	if d.donut == nil {
		return 0, iodine.New(drivers.InternalError{}, nil)
	}
	errParams := map[string]string{
		"bucketName": bucketName,
		"objectName": objectName,
		"start":      strconv.FormatInt(start, 10),
		"length":     strconv.FormatInt(length, 10),
	}
	if !drivers.IsValidBucket(bucketName) || strings.Contains(bucketName, ".") {
		return 0, iodine.New(drivers.BucketNameInvalid{Bucket: bucketName}, errParams)
	}
	if !drivers.IsValidObjectName(objectName) || strings.TrimSpace(objectName) == "" {
		return 0, iodine.New(drivers.ObjectNameInvalid{Object: objectName}, errParams)
	}
	if start < 0 {
		return 0, iodine.New(drivers.InvalidRange{
			Start:  start,
			Length: length,
		}, errParams)
	}
	reader, size, err := d.donut.GetObject(bucketName, objectName)
	if err != nil {
		return 0, iodine.New(drivers.ObjectNotFound{
			Bucket: bucketName,
			Object: objectName,
		}, nil)
	}
	defer reader.Close()
	if start > size || (start+length-1) > size {
		return 0, iodine.New(drivers.InvalidRange{
			Start:  start,
			Length: length,
		}, errParams)
	}
	_, err = io.CopyN(ioutil.Discard, reader, start)
	if err != nil {
		return 0, iodine.New(err, errParams)
	}
	n, err := io.CopyN(w, reader, length)
	if err != nil {
		return 0, iodine.New(err, errParams)
	}
	return n, nil
}

// GetObjectMetadata retrieves an object's metadata
func (d donutDriver) GetObjectMetadata(bucketName, objectName string) (drivers.ObjectMetadata, error) {
	d.lock.RLock()
	defer d.lock.RUnlock()

	errParams := map[string]string{
		"bucketName": bucketName,
		"objectName": objectName,
	}
	if d.donut == nil {
		return drivers.ObjectMetadata{}, iodine.New(drivers.InternalError{}, errParams)
	}
	if !drivers.IsValidBucket(bucketName) || strings.Contains(bucketName, ".") {
		return drivers.ObjectMetadata{}, iodine.New(drivers.BucketNameInvalid{Bucket: bucketName}, errParams)
	}
	if !drivers.IsValidObjectName(objectName) || strings.TrimSpace(objectName) == "" {
		return drivers.ObjectMetadata{}, iodine.New(drivers.ObjectNameInvalid{Object: objectName}, errParams)
	}
	metadata, err := d.donut.GetObjectMetadata(bucketName, objectName)
	if err != nil {
		return drivers.ObjectMetadata{}, iodine.New(drivers.ObjectNotFound{
			Bucket: bucketName,
			Object: objectName,
		}, errParams)
	}
	objectMetadata := drivers.ObjectMetadata{
		Bucket: bucketName,
		Key:    objectName,

		ContentType: metadata.Metadata["contentType"],
		Created:     metadata.Created,
		Md5:         metadata.MD5Sum,
		Size:        metadata.Size,
	}
	return objectMetadata, nil
}

type byObjectKey []drivers.ObjectMetadata

func (b byObjectKey) Len() int           { return len(b) }
func (b byObjectKey) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byObjectKey) Less(i, j int) bool { return b[i].Key < b[j].Key }

// ListObjects - returns list of objects
func (d donutDriver) ListObjects(bucketName string, resources drivers.BucketResourcesMetadata) ([]drivers.ObjectMetadata, drivers.BucketResourcesMetadata, error) {
	d.lock.RLock()
	defer d.lock.RUnlock()
	errParams := map[string]string{
		"bucketName": bucketName,
	}
	if d.donut == nil {
		return nil, drivers.BucketResourcesMetadata{}, iodine.New(drivers.InternalError{}, errParams)
	}
	if !drivers.IsValidBucket(bucketName) || strings.Contains(bucketName, ".") {
		return nil, drivers.BucketResourcesMetadata{}, iodine.New(drivers.BucketNameInvalid{Bucket: bucketName}, nil)
	}
	if !drivers.IsValidObjectName(resources.Prefix) {
		return nil, drivers.BucketResourcesMetadata{}, iodine.New(drivers.ObjectNameInvalid{Object: resources.Prefix}, nil)
	}
	actualObjects, commonPrefixes, isTruncated, err := d.donut.ListObjects(bucketName, resources.Prefix, resources.Marker, resources.Delimiter,
		resources.Maxkeys)
	if err != nil {
		return nil, drivers.BucketResourcesMetadata{}, iodine.New(err, errParams)
	}
	resources.CommonPrefixes = commonPrefixes
	resources.IsTruncated = isTruncated
	if resources.IsTruncated && resources.IsDelimiterSet() {
		resources.NextMarker = actualObjects[len(actualObjects)-1]
	}
	var results []drivers.ObjectMetadata
	for _, objectName := range actualObjects {
		objectMetadata, err := d.donut.GetObjectMetadata(bucketName, objectName)
		if err != nil {
			return nil, drivers.BucketResourcesMetadata{}, iodine.New(err, errParams)
		}
		metadata := drivers.ObjectMetadata{
			Key:     objectName,
			Created: objectMetadata.Created,
			Size:    objectMetadata.Size,
		}
		results = append(results, metadata)
	}
	sort.Sort(byObjectKey(results))
	return results, resources, nil
}

// CreateObject creates a new object
func (d donutDriver) CreateObject(bucketName, objectName, contentType, expectedMD5Sum string, size int64, reader io.Reader) (string, error) {
	d.lock.Lock()
	defer d.lock.Unlock()
	errParams := map[string]string{
		"bucketName":  bucketName,
		"objectName":  objectName,
		"contentType": contentType,
	}
	if d.donut == nil {
		return "", iodine.New(drivers.InternalError{}, errParams)
	}
	if !drivers.IsValidBucket(bucketName) || strings.Contains(bucketName, ".") {
		return "", iodine.New(drivers.BucketNameInvalid{Bucket: bucketName}, nil)
	}
	if !drivers.IsValidObjectName(objectName) || strings.TrimSpace(objectName) == "" {
		return "", iodine.New(drivers.ObjectNameInvalid{Object: objectName}, nil)
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}
	metadata := make(map[string]string)
	metadata["contentType"] = strings.TrimSpace(contentType)
	metadata["contentLength"] = strconv.FormatInt(size, 10)

	if strings.TrimSpace(expectedMD5Sum) != "" {
		expectedMD5SumBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(expectedMD5Sum))
		if err != nil {
			return "", iodine.New(err, nil)
		}
		expectedMD5Sum = hex.EncodeToString(expectedMD5SumBytes)
	}
	calculatedMD5Sum, err := d.donut.PutObject(bucketName, objectName, expectedMD5Sum, ioutil.NopCloser(reader), metadata)
	if err != nil {
		return "", iodine.New(err, errParams)
	}
	return calculatedMD5Sum, nil
}

func (d donutDriver) ListMultipartUploads(bucket string, resources drivers.BucketMultipartResourcesMetadata) (drivers.BucketMultipartResourcesMetadata, error) {
	return drivers.BucketMultipartResourcesMetadata{}, iodine.New(drivers.APINotImplemented{API: "ListMultipartUploads"}, nil)
}

func (d donutDriver) NewMultipartUpload(bucket, key, contentType string) (string, error) {
	return "", iodine.New(drivers.APINotImplemented{API: "NewMultipartUpload"}, nil)
}

func (d donutDriver) CreateObjectPart(bucket, key, uploadID string, partID int, contentType, expectedMD5Sum string, size int64, data io.Reader) (string, error) {
	return "", iodine.New(drivers.APINotImplemented{API: "CreateObjectPart"}, nil)
}

func (d donutDriver) CompleteMultipartUpload(bucket, key, uploadID string, parts map[int]string) (string, error) {
	return "", iodine.New(drivers.APINotImplemented{API: "CompleteMultipartUpload"}, nil)
}

func (d donutDriver) ListObjectParts(bucket, key string, resources drivers.ObjectResourcesMetadata) (drivers.ObjectResourcesMetadata, error) {
	return drivers.ObjectResourcesMetadata{}, iodine.New(drivers.APINotImplemented{API: "ListObjectParts"}, nil)
}

func (d donutDriver) AbortMultipartUpload(bucket, key, uploadID string) error {
	return iodine.New(drivers.APINotImplemented{API: "AbortMultipartUpload"}, nil)
}
