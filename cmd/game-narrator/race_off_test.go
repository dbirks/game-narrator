//go:build !race

package main

// ocrEngineBudgetScale is 1 in an uninstrumented build: the one-shot ocr
// command's real-Tesseract tests run the engine's production default
// budgets there. See race_on_test.go for why the race build scales them.
const ocrEngineBudgetScale = 1
