package tui

// Braille renderings of the Iris star brand-mark (site /brand-mark.png,
// 166x256 black-on-transparent). Generated offline: resize to cols*2 x rows*4
// (LANCZOS), light dot when alpha >= threshold, map each 2x4 pixel block to
// U+2800+mask with bit b at offset [(0,0),(0,1),(0,2),(1,0),(1,1),(1,2),(0,3),(1,3)][b].
// Blank cells stay ' ' (never U+2800) and rows are right-trimmed so plain
// frames golden cleanly. Treated as immutable.

// logoSplash is the 16x12 welcome-card mark (threshold 128).
var logoSplash = []string{
	"       ⢰⡆",
	"       ⢸⡇",
	"       ⢸⡇",
	"⣀      ⢸⣷⡀     ⣀",
	"⠈⠙⢶⣦⣤⣄⡀⢸⣿⣿⣶⣦⣴⡶⠋⠁",
	"   ⢹⣿⣿⣿⣼⣿⣿⣿⣿⡏",
	"   ⣸⣿⣿⣿⣿⣿⣿⣿⣿⣇",
	"⢀⣠⡴⠿⠿⢿⣿⣿⣿⣿⡿⠿⠿⢶⣄⡀",
	"⠉     ⠈⢿⡿⠁    ⠈⠉",
	"       ⢸⡇",
	"       ⢸⡇",
	"       ⠸⠇",
}

// logoWidth is the widest rune count across the art's rows.
func logoWidth(art []string) int {
	w := 0
	for _, row := range art {
		if n := len([]rune(row)); n > w {
			w = n
		}
	}
	return w
}
