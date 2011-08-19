package main

import (
	"log"
	"os"
	"strings"
)

type env struct {
	*BasicEnvelope
}

func (e *env) AddRecipient(rcpt MailAddress) os.Error {
	if strings.HasPrefix(rcpt.Email(), "bad@") {
		return os.NewError("we don't send email to bad@")
	}
	return e.BasicEnvelope.AddRecipient(rcpt)
}

func onNewMail(c Connection, from MailAddress) (Envelope, os.Error) {
	log.Printf("ajas: new mail from %q", from)
	return &env{new(BasicEnvelope)}, nil
}

func main() {
	s := &Server{
		Addr:      ":2500",
		OnNewMail: onNewMail,
	}
	err := s.ListenAndServe()
	if err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
