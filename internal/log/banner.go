package log

import "fmt"

const banner = `
    ___    ____  ____  ____     _____                          
   /   |  / __ \/ __ \/ __ \   / ___/___  ______   _____  _____
  / /| | / /_/ / / / / / / /   \__ \/ _ \/ ___/ | / / _ \/ ___/
 / ___ |/ ____/ /_/ / /_/ /   ___/ /  __/ /   | |/ /  __/ /    
/_/  |_/_/    \____/_____/   /____/\___/_/    |___/\___/_/     

`

// PrintBanner prints the ASCII art banner to stdout.
func PrintBanner() {
	fmt.Print(banner)
}
