// bricolage - a content management system
// Copyright (c) 2023 Michael D Henderson
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package cli

import (
	"github.com/mdhender/bricolage"
	"github.com/spf13/cobra"
	"log"
)

var rootCmd = &cobra.Command{
	Use:   "bricolage",
	Short: "Bricolage is a content management system",
	Long:  `A CMS derived from a better CMS, Bricolage CMS.`,
	Run:   func(cmd *cobra.Command, args []string) {},
}

var config *bricolage.Config

func Execute() {
	config = bricolage.DefaultConfig()

	rootCmd.Flags().StringVar(&config.Content, "content", config.Content, "Path to read content from")
	rootCmd.Flags().StringVar(&config.Site, "site", config.Site, "Path to write generated site files")

	rootCmd.AddCommand(aboutCmd)

	serverCmd.Flags().IntVarP(&config.Server.Port, "port", "p", config.Server.Port, "Port to listen on")
	rootCmd.AddCommand(serverCmd)

	rootCmd.AddCommand(versionCmd)

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}
