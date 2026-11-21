package main

import "strings"

const (
	deepSeekFlash1017Identity = "deepseek-v-4-1-flash-20261017"
	deepSeekFlash0910Identity = "deepseek-v-4-1-flash-20260910"
)

// Explicit, verified date spellings only. This does not erase thinking modes,
// equate arbitrary generations, or resolve rolling third-party aliases.
func arenaVersionedIdentity(model string) string {
	key := arenaModelIdentity(model)
	for _, pair := range [][2]string{
		{"deepseek-v-4-1-flash-1017", deepSeekFlash1017Identity},
		{"deepseek-v-4-1-flash-0910", deepSeekFlash0910Identity},
	} {
		if key == pair[0] || strings.HasPrefix(key, pair[0]+"-") {
			return pair[1] + strings.TrimPrefix(key, pair[0])
		}
	}
	return key
}
