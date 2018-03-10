/*
 * Copyright 2017-2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"). You
 * may not use this file except in compliance with the License. A copy of
 * the License is located at
 *
 * 	http://aws.amazon.com/apache2.0/
 *
 * or in the "license" file accompanying this file. This file is
 * distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF
 * ANY KIND, either express or implied. See the License for the specific
 * language governing permissions and limitations under the License.
 */
package ecr

import (
	"context"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

var (
	errLayerNotFound = errors.New("ecr: layer not found")
)

// ecrPusher implements the containerd remotes.Pusher interface and can be used
// to push images to Amazon ECR.
type ecrPusher struct {
	ecrBase
}

var _ remotes.Pusher = (*ecrPusher)(nil)

func (p ecrPusher) Push(ctx context.Context, desc ocispec.Descriptor) (content.Writer, error) {
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("desc", desc))
	log.G(ctx).Debug("ecr.push")

	switch desc.MediaType {
	case
		ocispec.MediaTypeImageManifest,
		images.MediaTypeDockerSchema2Manifest,
		images.MediaTypeDockerSchema1Manifest:
		return p.pushManifest(ctx, desc)
	default:
		return p.pushBlob(ctx, desc)
	}

	return nil, unimplemented
}

func (p ecrPusher) pushManifest(ctx context.Context, desc ocispec.Descriptor) (content.Writer, error) {
	log.G(ctx).Debug("ecr.pusher.manifest")
	exists, err := p.checkManifestExistence(ctx, desc)
	if err != nil {
		log.G(ctx).WithError(err).
			Error("ecr.pusher.manifest: failed to check existence")
		return nil, err
	}
	if exists {
		log.G(ctx).Debug("ecr.pusher.manifest: content already on remote")
		return nil, errors.Wrapf(errdefs.ErrAlreadyExists, "content %v on remote", desc.Digest)
	}

	return &manifestWriter{
		ctx:  ctx,
		base: &p.ecrBase,
		desc: desc,
	}, nil
}

func (p ecrPusher) checkManifestExistence(ctx context.Context, desc ocispec.Descriptor) (bool, error) {
	image, err := p.getManifest(ctx)
	if err != nil {
		if err == errImageNotFound {
			return false, nil
		}
		return false, err
	}
	if image == nil {
		return false, errors.New("ecr.pusher.manifest: unexpected nil image")
	}

	found := desc.Digest.String() == aws.StringValue(image.ImageId.ImageDigest)
	return found, nil
}

func (p ecrPusher) pushBlob(ctx context.Context, desc ocispec.Descriptor) (content.Writer, error) {
	log.G(ctx).Debug("ecr.pusher.blob")
	exists, err := p.checkBlobExistence(ctx, desc)
	if err != nil {
		log.G(ctx).WithError(err).
			Error("ecr.pusher.blob: failed to check existence")
		return nil, err
	}
	if exists {
		log.G(ctx).Debug("ecr.pusher.blob: content already on remote")
		return nil, errors.Wrapf(errdefs.ErrAlreadyExists, "content %v on remote", desc.Digest)
	}

	return newLayerWriter(&p.ecrBase, desc)
}

func (p ecrPusher) checkBlobExistence(ctx context.Context, desc ocispec.Descriptor) (bool, error) {
	batchCheckLayerAvailabilityInput := &ecr.BatchCheckLayerAvailabilityInput{
		RegistryId:     aws.String(p.ecrSpec.Registry()),
		RepositoryName: aws.String(p.ecrSpec.Repository),
		LayerDigests:   []*string{aws.String(desc.Digest.String())},
	}

	batchCheckLayerAvailabilityOutput, err := p.client.BatchCheckLayerAvailability(batchCheckLayerAvailabilityInput)
	if err != nil {
		log.G(ctx).WithError(err).Error("ecr.pusher.blob: failed to check availability")
		return false, err
	}
	log.G(ctx).
		WithField("batchCheckLayerAvailability", batchCheckLayerAvailabilityOutput).
		Debug("ecr.pusher.blob")

	if len(batchCheckLayerAvailabilityOutput.Layers) != 1 {
		if len(batchCheckLayerAvailabilityOutput.Failures) > 0 {
			return false, errLayerNotFound
		}
		return false, reference.ErrInvalid
	}

	layer := batchCheckLayerAvailabilityOutput.Layers[0]
	return aws.StringValue(layer.LayerAvailability) == ecr.LayerAvailabilityAvailable, nil
}