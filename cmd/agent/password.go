package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// readNewPassword gets the browser interface's new password from the
// environment, from the terminal without echo, or from a pipe -- in that
// order, because an installer sets the variable, an operator types, and a
// script pipes.
func readNewPassword(stdin *os.File, prompt io.Writer) (string, error) {
	if fromEnv, given := os.LookupEnv("GNIZA_WEB_PASSWORD"); given {
		return strings.TrimRight(fromEnv, "\r\n"), nil
	}
	if isTerminal(stdin) {
		first, err := readSecret(stdin, prompt, "Password for the browser interface: ")
		if err != nil {
			return "", err
		}
		again, err := readSecret(stdin, prompt, "The same again: ")
		if err != nil {
			return "", err
		}
		if first != again {
			return "", errors.New("the two passwords differ")
		}
		return first, nil
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func isTerminal(file *os.File) bool {
	_, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	return err == nil
}

// readSecret reads one line with the terminal's echo off, and puts the
// echo back however the read ends.
func readSecret(terminal *os.File, prompt io.Writer, question string) (string, error) {
	fd := int(terminal.Fd())
	saved, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", err
	}
	quiet := *saved
	quiet.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return "", err
	}
	defer func() {
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, saved)
		fmt.Fprintln(prompt)
	}()
	fmt.Fprint(prompt, question)
	line, err := bufio.NewReader(terminal).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
