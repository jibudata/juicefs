//go:build !noazure
// +build !noazure

/*
 * JuiceFS, Copyright 2018 Juicedata, Inc.
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

package object

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

type wasb struct {
	DefaultObjectStorage
	container *azblob.ContainerClient
	cName     string
	marker    string
}

func (b *wasb) String() string {
	return fmt.Sprintf("wasb://%s/", b.cName)
}

func (b *wasb) Create() error {
	_, err := b.container.Create(ctx, nil)
	if err != nil && strings.Contains(err.Error(), string(azblob.StorageErrorCodeContainerAlreadyExists)) {
		return nil
	}
	return err
}

func (b *wasb) Head(key string) (Object, error) {
	properties, err := b.container.NewBlobClient(key).GetProperties(ctx, &azblob.GetBlobPropertiesOptions{})
	if err != nil {
		return nil, err
	}

	return &obj{
		key,
		*properties.ContentLength,
		*properties.LastModified,
		strings.HasSuffix(key, "/"),
	}, nil
}

func (b *wasb) Get(key string, off, limit int64) (io.ReadCloser, error) {
	download, err := b.container.NewBlockBlobClient(key).Download(ctx, &azblob.DownloadBlobOptions{Offset: &off, Count: &limit})
	if err != nil {
		return nil, err
	}
	return download.BlobDownloadResponse.RawResponse.Body, err
}

func (b *wasb) Put(key string, data io.Reader) error {
	_, err := b.container.NewBlockBlobClient(key).UploadStreamToBlockBlob(ctx, data, azblob.UploadStreamToBlockBlobOptions{})
	return err
}

func (b *wasb) Copy(dst, src string) error {
	_, err := b.container.NewBlockBlobClient(dst).CopyFromURL(ctx, b.container.NewBlockBlobClient(src).URL(),
		&azblob.CopyBlockBlobFromURLOptions{})
	return err
}

func (b *wasb) Delete(key string) error {
	_, err := b.container.NewBlockBlobClient(key).Delete(ctx, &azblob.DeleteBlobOptions{})
	if err != nil && strings.Contains(err.Error(), string(azblob.StorageErrorCodeBlobNotFound)) {
		return nil
	}
	return err
}

func (b *wasb) List(prefix, marker string, limit int64) ([]Object, error) {
	if marker != "" {
		if b.marker == "" {
			// last page
			return nil, nil
		}
		marker = b.marker
	}

	limit32 := int32(limit)
	pager := b.container.ListBlobsFlat(&azblob.ContainerListBlobFlatSegmentOptions{Prefix: &prefix, Marker: &marker, Maxresults: &(limit32)})
	if pager.NextPage(ctx) {
		b.marker = *pager.PageResponse().NextMarker
	} else {
		b.marker = ""
	}
	n := len(pager.PageResponse().Segment.BlobItems)
	objs := make([]Object, n)
	for i := 0; i < n; i++ {
		blob := pager.PageResponse().Segment.BlobItems[i]
		mtime := blob.Properties.LastModified
		objs[i] = &obj{
			*blob.Name,
			*blob.Properties.ContentLength,
			*mtime,
			strings.HasSuffix(*blob.Name, "/"),
		}
	}
	return objs, nil
}

func autoWasbEndpoint(containerName, accountName, scheme string, credential *azblob.SharedKeyCredential) (string, error) {
	baseURLs := []string{"blob.core.windows.net", "blob.core.chinacloudapi.cn"}
	endpoint := ""
	for _, baseURL := range baseURLs {
		if _, err := net.LookupIP(fmt.Sprintf("%s.%s", accountName, baseURL)); err != nil {
			logger.Debugf("Attempt to resolve domain name %s failed: %s", baseURL, err)
			continue
		}
		client, err := azblob.NewContainerClientWithSharedKey(fmt.Sprintf("%s://%s.%s/%s", scheme, accountName, baseURL, containerName), credential, nil)
		if err != nil {
			return "", err
		}
		if _, err = client.GetProperties(ctx, nil); err != nil {
			logger.Debugf("Try to get containers properties at %s failed: %s", baseURL, err)
			continue
		}
		endpoint = baseURL
		break
	}

	if endpoint == "" {
		return "", fmt.Errorf("fail to get endpoint for container %s", containerName)
	}
	return endpoint, nil
}

func newWabs(endpoint, accountName, accountKey string) (ObjectStorage, error) {
	if !strings.Contains(endpoint, "://") {
		endpoint = fmt.Sprintf("https://%s", endpoint)
	}
	uri, err := url.ParseRequestURI(endpoint)
	if err != nil {
		return nil, fmt.Errorf("Invalid endpoint: %v, error: %v", endpoint, err)
	}
	hostParts := strings.SplitN(uri.Host, ".", 2)

	scheme := ""
	domain := ""
	// Connection string support: DefaultEndpointsProtocol=[http|https];AccountName=***;AccountKey=***;EndpointSuffix=[core.windows.net|core.chinacloudapi.cn]
	if connString := os.Getenv("AZURE_STORAGE_CONNECTION_STRING"); connString != "" {
		items := strings.Split(connString, ";")
		for _, item := range items {
			if item = strings.TrimSpace(item); item == "" {
				continue
			}
			parts := strings.SplitN(item, "=", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("Invalid connection string item: %s", item)
			}
			// Arguments from command line take precedence
			if parts[0] == "DefaultEndpointsProtocol" && scheme == "" {
				scheme = parts[1]
			} else if parts[0] == "AccountName" && accountName == "" {
				accountName = parts[1]
			} else if parts[0] == "AccountKey" && accountKey == "" {
				accountKey = parts[1]
			} else if parts[0] == "EndpointSuffix" && domain == "" {
				domain = parts[1]
			}
		}
	}
	credential, err := azblob.NewSharedKeyCredential(accountName, accountKey)
	if err != nil {
		return nil, err
	}

	if scheme == "" {
		scheme = "https"
	}
	containerName := hostParts[0]
	if len(hostParts) > 1 {
		// Arguments from command line take precedence
		domain = hostParts[1]
	} else if domain == "" {
		if domain, err = autoWasbEndpoint(containerName, accountName, scheme, credential); err != nil {
			return nil, fmt.Errorf("Unable to get endpoint of container %s: %s", containerName, err)
		}
	}

	client, err := azblob.NewContainerClientWithSharedKey(fmt.Sprintf("%s://%s.%s/%s", scheme, accountName, domain, containerName), credential, nil)
	if err != nil {
		return nil, err
	}

	return &wasb{container: &client, cName: containerName}, nil
}

func init() {
	Register("wasb", newWabs)
}
