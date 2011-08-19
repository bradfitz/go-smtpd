package main

import (
	"log"
)

func main() {
	s := &Server{
		Addr: ":2500",
	}
	err := s.ListenAndServe()
	if err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
