VERSION   ?= dev
IMAGE     ?= ghcr.io/kindacoolhamster/bard-csi:$(VERSION)
IMAGE_TAR ?= /tmp/bard-csi-image.tar
CLUSTER   ?= bard
GO        ?= go
KIND      ?= $(HOME)/.local/bin/kind

REGISTRY ?= ghcr.io/kindacoolhamster

.PHONY: build test vet fmt image images clean kind-up kind-down images-load deploy redeploy e2e

## --- code ---
build:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o bin/bard-csi ./cmd/bard-csi

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

## --- images (rootless podman): core + the backend plugins ---
image:
	podman build --build-arg VERSION=$(VERSION) -t $(IMAGE) .
	rm -f $(IMAGE_TAR)
	podman save -o $(IMAGE_TAR) $(IMAGE)

# The same seven images the release workflow publishes (.github/workflows/images.yaml).
PLUGINS ?= ceph-rbd cephfs nfs lvm iscsi localpath

images: image  ## build core + every backend plugin image, save tars
	for p in $(PLUGINS); do \
	  podman build -f Dockerfile.plugin-$$p --build-arg VERSION=$(VERSION) -t $(REGISTRY)/bard-plugin-$$p:$(VERSION) . || exit 1; \
	  rm -f /tmp/bard-plugin-$$p.tar; \
	  podman save -o /tmp/bard-plugin-$$p.tar $(REGISTRY)/bard-plugin-$$p:$(VERSION) || exit 1; \
	done

## --- local end-to-end harness (rootful kind, see CLAUDE.md) ---
kind-up:            ## create the cluster + all host fixes
	sudo bash hack/setup-rootful-kind.sh

kind-down:          ## delete the cluster
	sudo bash hack/setup-rootful-kind.sh delete

images-load:        ## load core + plugin images into the cluster
	sudo env KIND_EXPERIMENTAL_PROVIDER=podman $(KIND) load image-archive $(IMAGE_TAR) --name $(CLUSTER)
	for p in $(PLUGINS); do \
	  sudo env KIND_EXPERIMENTAL_PROVIDER=podman $(KIND) load image-archive /tmp/bard-plugin-$$p.tar --name $(CLUSTER) || exit 1; \
	done

deploy:             ## apply manifests + real secret (needs KUBECONFIG=~/.kube/config-bard)
	kubectl apply -f deploy/
	kubectl apply -f hack/secret.yaml

redeploy: images images-load   ## rebuild+reload all images, then restart the driver pods
	kubectl -n kube-system delete pod -l 'app in (bard-csi-controller,bard-csi-node)'

e2e:                ## PVC smoke test: provision -> mount -> read proof file
	kubectl apply -f hack/test-pvc.yaml
	kubectl wait --for=condition=Ready pod/bard-test --timeout=150s
	kubectl exec bard-test -- cat /data/proof.txt

clean:
	rm -rf bin
