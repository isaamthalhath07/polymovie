package main

import (
	"context"
	"fmt"
	"log"
	"os"
	
	"time"

	"github.com/chromedp/chromedp"
)

func main() {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(`C:\Users\isaam\AppData\Local\Temp\polymovie-profile-debug`),
		chromedp.Flag("headless", true), // try headless just for DOM dump
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel2 := chromedp.NewContext(allocCtx)
	defer cancel2()

	err := chromedp.Run(ctx,
		chromedp.Navigate("https://www.amazon.co.uk/gp/video/detail/B0DWSGK5GS/"),
		chromedp.Sleep(5*time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}

	var html string
	err = chromedp.Run(ctx,
		chromedp.Evaluate(`
			Array.from(document.querySelectorAll('a, button')).map(el => {
				return (el.tagName + " | " + (el.href || "") + " | " + (el.innerText || "").replace(/\n/g, ' '));
			}).join('\n');
		`, &html),
	)
	if err != nil {
		log.Fatal(err)
	}
	os.WriteFile("dom_dump.txt", []byte(html), 0644)
	fmt.Println("Dumped DOM to dom_dump.txt")
}
