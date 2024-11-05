package main

import (
	"flag"
	"fmt"
	"os"
)

var gitUrl = flag.String("gitUrl", os.Getenv("KOHEN_GIT_URL"), "URL to the source config git repository")
var gitPath = flag.String("gitPath", os.Getenv("KOHEN_GIT_PATH"), "A path within git repository")

func main() {
	fmt.Println("kohen-agent is starting")
	flag.Parse()
	fmt.Println("git repository: ", *gitUrl)
	fmt.Println("git path: ", *gitPath)
	fmt.Println("kohen-agent complete")
}
