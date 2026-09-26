package downloads

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
)

// XMPData is the metadata written to a sidecar file.
type XMPData struct {
	Rating   int      // 0 = unrated, otherwise 1-5
	Tags     []string // includes the uploader/<nick> tag
	Uploader string   // uploader nickname
}

// BuildXMP renders a standard XMP sidecar understood by Lightroom, darktable,
// digiKam and similar tools.
func BuildXMP(d XMPData) []byte {
	var b bytes.Buffer
	esc := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	b.WriteString("<?xpacket begin=\"\uFEFF\" id=\"W5M0MpCehiHzreSzNTczkc9d\"?>\n")
	b.WriteString(`<x:xmpmeta xmlns:x="adobe:ns:meta/">` + "\n")
	b.WriteString(` <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` + "\n")
	b.WriteString(`  <rdf:Description rdf:about=""` + "\n")
	b.WriteString(`    xmlns:xmp="http://ns.adobe.com/xap/1.0/"` + "\n")
	b.WriteString(`    xmlns:dc="http://purl.org/dc/elements/1.1/"` + "\n")
	b.WriteString(`    xmlns:lr="http://ns.adobe.com/lightroom/1.0/"`)
	if d.Rating >= 1 && d.Rating <= 5 {
		b.WriteString("\n    xmp:Rating=\"" + strconv.Itoa(d.Rating) + "\"")
	}
	b.WriteString(">\n")
	if len(d.Tags) > 0 {
		b.WriteString("   <dc:subject>\n    <rdf:Bag>\n")
		for _, t := range d.Tags {
			b.WriteString("     <rdf:li>" + esc(t) + "</rdf:li>\n")
		}
		b.WriteString("    </rdf:Bag>\n   </dc:subject>\n")
		b.WriteString("   <lr:hierarchicalSubject>\n    <rdf:Bag>\n")
		for _, t := range d.Tags {
			b.WriteString("     <rdf:li>" + esc(strings.ReplaceAll(t, "/", "|")) + "</rdf:li>\n")
		}
		b.WriteString("    </rdf:Bag>\n   </lr:hierarchicalSubject>\n")
	}
	if d.Uploader != "" {
		b.WriteString("   <dc:creator>\n    <rdf:Seq>\n     <rdf:li>" + esc(d.Uploader) + "</rdf:li>\n    </rdf:Seq>\n   </dc:creator>\n")
	}
	b.WriteString("  </rdf:Description>\n </rdf:RDF>\n</x:xmpmeta>\n")
	b.WriteString("<?xpacket end=\"w\"?>\n")
	return b.Bytes()
}
