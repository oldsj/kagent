/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	"github.com/kagent-dev/kagent/go/core/pkg/app"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/kagent-dev/kagent/go/core/pkg/env"
)

func main() {
	if err := app.SetupLogger(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logger := slog.Default()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	authenticator, err := controllerAuthenticator(env.AuthMode.Get(), env.AuthUserIDClaim.Get())
	if err != nil {
		logger.ErrorContext(ctx, "invalid controller authentication configuration", "error", err)
		os.Exit(1)
	}
	if err := app.Run(ctx, app.Options{Authenticator: authenticator}); err != nil {
		logger.ErrorContext(ctx, "controller stopped", "error", err)
		os.Exit(1)
	}
}

func controllerAuthenticator(mode, userIDClaim string) (auth.AuthProvider, error) {
	switch mode {
	case env.AuthModeInsecure:
		return &authimpl.InsecureAuthenticator{}, nil
	case env.AuthModeServiceToken:
		policy, err := controllerServicePolicy()
		if err != nil {
			return nil, err
		}
		return authimpl.NewServiceTokenAuthenticator(env.AuthServiceTokenCurrentFile.Get(), env.AuthServiceTokenNextFile.Get(), policy)
	case env.AuthModeTrustedProxy:
		return authimpl.NewProxyAuthenticator(userIDClaim), nil
	default:
		return nil, fmt.Errorf("unsupported %s %q: expected %s, %s or %s", env.AuthMode.Name(), mode, env.AuthModeInsecure, env.AuthModeTrustedProxy, env.AuthModeServiceToken)
	}
}

func controllerServicePolicy() (*controlauth.Policy, error) {
	var config controlauth.Config
	decoder := json.NewDecoder(strings.NewReader(env.AuthServicePolicy.Get()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("invalid service policy configuration")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("invalid service policy configuration")
	}
	return controlauth.New(config)
}
