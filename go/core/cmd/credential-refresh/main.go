package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/kagent-dev/kagent/go/core/internal/controller/chatgptrefresh"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	namespace := flag.String("namespace", "", "Credential namespace")
	secret := flag.String("secret", "", "Credential Secret name")
	key := flag.String("access-key", "", "Gateway access-token key")
	claim := flag.String("claim", "", "Controller rotation claim")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	config, err := rest.InClusterConfig()
	if err != nil {
		fail()
	}
	scheme := runtime.NewScheme()
	if corev1.AddToScheme(scheme) != nil {
		fail()
	}
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		fail()
	}
	if chatgptrefresh.RunJob(ctx, kube, types.NamespacedName{Namespace: *namespace, Name: *secret}, *key, *claim, "/work/codex", "/usr/local/bin/codex") != nil {
		fail()
	}
}

func fail() {
	// No upstream errors, request/response bodies, or auth contents escape.
	fmt.Fprintln(os.Stderr, "ChatGPT credential refresh did not complete")
	os.Exit(1)
}
