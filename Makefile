.PHONY: test build install enable

test:
	gofmt -l .
	go vet ./...
	go test ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/amp-orb-updater .

install: build
	install -Dm755 bin/amp-orb-updater $(HOME)/.local/bin/amp-orb-updater
	install -Dm644 deploy/amp-orb-updater.service $(HOME)/.config/systemd/user/amp-orb-updater.service
	install -Dm644 deploy/amp-orb-updater.timer $(HOME)/.config/systemd/user/amp-orb-updater.timer
	systemctl --user daemon-reload

enable: install
	systemctl --user enable --now amp-orb-updater.timer
