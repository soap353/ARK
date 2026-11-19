//go:build !windows

package main

func registerConsoleCloseHandler(trigger func(), done <-chan struct{}) {}
