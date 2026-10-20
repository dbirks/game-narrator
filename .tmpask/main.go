package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

func main() {
	url := os.Getenv(providers.EnvGatewayURL)
	tokenEnv := os.Getenv(providers.EnvGatewayTokenEnv)
	if tokenEnv == "" { tokenEnv = providers.EnvGatewayToken }
	token := os.Getenv(tokenEnv)
	model := os.Getenv("GAME_NARRATOR_LIVE_MODEL")
	if model == "" { model = "qwen3-8-27b" }
	c, err := openai.New(url, token, model)
	if err != nil { panic(err) }
	c.HTTP = nil
	data, err := os.ReadFile(os.Args[1])
	if err != nil { panic(err) }
	var q string
	switch os.Args[2] {
	case "scene":
		q = "Describe this Wind Waker frame in at most two sentences: what is on screen (scene, characters, any text box, subtitle or button prompt and where), and what is the main character doing?"
	case "arrow":
		q = "This is a frame from the GameCube game The Legend of Zelda: The Wind Waker. " +
			"Where is the floating green talk arrow that appears above an NPC you can talk to — bottom-left, bottom-centre, bottom-right, middle-left, middle-right, centre, or none (not visible)? " +
			"Answer with exactly one word."
	case "prompt":
		q = "This is a frame from The Legend of Zelda: The Wind Waker. Is a button action-prompt label visible (an A-button icon with a word next to it)? If yes, what word does it read and where is it (left/centre/right)? Answer in at most five words."
	}
	resp, err := c.Chat(context.Background(), openai.ChatRequest{
		Messages: []openai.Message{{Role: "user", Text: q, Images: [][]byte{data}}},
		MaxTokens: 64,
		ReasoningEffort: "none",
	})
	if err != nil { panic(err) }
	fmt.Println(strings.TrimSpace(resp.Text))
}
