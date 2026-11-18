package isobmff

import "strings"

var udtaNames = map[string]string{
	"\xa9nam": "title", "\xa9ART": "artist", "\xa9alb": "album", "\xa9day": "date",
	"\xa9too": "encoder", "\xa9cmt": "comment", "\xa9gen": "genre", "\xa9wrt": "composer",
	"\xa9swr": "encoder", "\xa9mak": "make", "\xa9mod": "model", "\xa9xyz": "location",
	"\xa9cpy": "copyright", "\xa9des": "description", "aART": "album_artist", "desc": "description",
}

func (f *File) setTag(k, v string) {
	v = strings.TrimRight(v, "\x00")
	if k == "" || v == "" || len(f.Tags) >= 256 || len(v) > 4096 {
		return
	}
	if _, dup := f.Tags[k]; !dup {
		f.Tags[k] = v
	}
}

func (f *File) parseUdta(b []byte) {
	walk(b, func(typ string, p []byte) error {
		if typ == "meta" {
			f.parseMeta(p)
			return nil
		}
		// QuickTime international text: uint16 length, uint16 language, text.
		if name, ok := udtaNames[typ]; ok && len(p) >= 4 {
			n := int(be.Uint16(p))
			if n <= len(p)-4 && n > 0 {
				f.setTag(name, string(p[4:4+n]))
			}
		}
		return nil
	})
}

// parseMeta handles both the ISO form (FullBox) and the QuickTime form (no
// version/flags), with iTunes-style 'ilst' or QuickTime 'keys' + 'ilst'.
func (f *File) parseMeta(b []byte) {
	if len(b) >= 8 && string(b[4:8]) != "hdlr" && len(b) >= 12 {
		b = b[4:] // skip version/flags
	}
	var keys []string
	walk(b, func(typ string, p []byte) error {
		switch typ {
		case "keys":
			if len(p) < 8 {
				return nil
			}
			q := p[8:]
			for len(q) >= 8 && len(keys) < 256 {
				n := int(be.Uint32(q))
				if n < 8 || n > len(q) {
					break
				}
				keys = append(keys, string(q[8:n]))
				q = q[n:]
			}
		case "ilst":
			walk(p, func(typ string, item []byte) error {
				name := udtaNames[typ]
				if len(keys) > 0 {
					if idx := int(be.Uint32([]byte(typ))); idx >= 1 && idx <= len(keys) {
						name = keys[idx-1]
					}
				}
				if name == "" {
					return nil
				}
				walk(item, func(typ string, d []byte) error {
					if typ == "data" && len(d) >= 8 {
						if dt := be.Uint32(d) & 0xffffff; dt == 1 || dt == 0 {
							f.setTag(name, string(d[8:]))
						}
					}
					return nil
				})
				return nil
			})
		}
		return nil
	})
}
