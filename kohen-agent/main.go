package main

import (
	"flag"
	"fmt"
	"os"
	"github.com/go-git/go-git/v5"
)

type Config struct {
	gitUrl    string
	gitPath   string
	targetDir string
}

func main() {
	fmt.Println("🚀 kohen-agent is starting")

	config := readConfig()

	fmt.Println("⦿ git repository 👉 ", config.gitUrl)
	fmt.Println("⦿ git path       👉 ", config.gitPath)
	fmt.Println("⦿ target dir     👉 ", config.targetDir)

	fetchRepo(config)

	fmt.Println("🏁 kohen-agent complete")
}

func readConfig() Config {
	gitUrl := flag.String("gitUrl", os.Getenv("KOHEN_GIT_URL"), "URL to the source config git repository")
	gitPath := flag.String("gitPath", os.Getenv("KOHEN_GIT_PATH"), "A path within git repository")
	targetDir := flag.String("targetDir", os.Getenv("KOHEN_TARGET_DIR"), "A target directory to place the config")

	flag.Parse()

	if gitUrl == nil || *gitUrl == "" {
		fmt.Sprintln("ERROR: gitUrl is not provided")
		fmt.Sprintln("Usage:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if targetDir == nil || *targetDir == "" {
		fmt.Sprintln("ERROR: targetDir is not provided")
		fmt.Sprintln("Usage:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	return Config{
		gitUrl:  *gitUrl,
		gitPath: *gitPath,
		targetDir: *targetDir,
	}
}

func fetchRepo(config Config) {
	_, err := git.PlainClone(config.targetDir, false, &git.CloneOptions{
		URL:      config.gitUrl,
		Progress: os.Stdout,
	})

	if err != nil {
		fmt.Sprintln("ERROR: " + err.Error())
		os.Exit(1)
	}
}
