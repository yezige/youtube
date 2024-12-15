package youtube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/dop251/goja"
)

func (c *Client) decipherURL(ctx context.Context, videoID string, cipher string) (string, error) {
	params, err := url.ParseQuery(cipher)
	if err != nil {
		return "", err
	}

	uri, err := url.Parse(params.Get("url"))
	if err != nil {
		return "", err
	}
	query := uri.Query()

	config, err := c.getPlayerConfig(ctx, videoID)
	if err != nil {
		return "", err
	}

	// decrypt s-parameter
	bs, err := config.decrypt([]byte(params.Get("s")))
	if err != nil {
		return "", err
	}
	query.Add(params.Get("sp"), string(bs))

	query, err = c.decryptNParam(config, query)
	if err != nil {
		return "", err
	}

	uri.RawQuery = query.Encode()

	return uri.String(), nil
}

// see https://github.com/kkdai/youtube/pull/244
func (c *Client) unThrottle(ctx context.Context, videoID string, urlString string) (string, error) {
	config, err := c.getPlayerConfig(ctx, videoID)
	if err != nil {
		return "", err
	}

	uri, err := url.Parse(urlString)
	if err != nil {
		return "", err
	}

	// for debugging
	if artifactsFolder != "" {
		writeArtifact("video-"+videoID+".url", []byte(uri.String()))
	}

	query, err := c.decryptNParam(config, uri.Query())
	if err != nil {
		return "", err
	}

	query, err = c.decryptSignParam(config, query)
	if err != nil {
		return "", err
	}

	uri.RawQuery = query.Encode()
	return uri.String(), nil
}

func (c *Client) decryptNParam(config playerConfig, query url.Values) (url.Values, error) {
	// decrypt n-parameter
	var n = "n"
	nSig := query.Get(n)
	log := Logger.With("n", nSig)

	if nSig != "" {
		nDecoded, err := config.decodeNsig(nSig)
		if err != nil {
			return nil, fmt.Errorf("unable to decode nSig: %w", err)
		}
		query.Set(n, nDecoded)
		log = log.With("decoded", nDecoded)
	}

	if c.client.name == "WEB" {
		query.Set("cver", c.client.version)
	}

	log.Debug("decryptNParam")

	return query, nil
}

// 计算签名
// youtube-cloud/youtube-js/node_modules/youtubei.js/dist/src/core/Player.js
func (c *Client) decryptSignParam(config playerConfig, query url.Values) (url.Values, error) {
	// decrypt s-parameter
	sign := query.Get("s")
	log := Logger.With("s", sign)

	if sign != "" {
		decoded, err := config.decodeSign(sign)
		if err != nil {
			return nil, fmt.Errorf("unable to decode sign: %w", err)
		}
		if query.Get("sp") != "" {
			query.Set(query.Get("sp"), decoded)
		} else {
			query.Set("signature", decoded)
		}
	
		log = log.With("decoded", decoded)
	}

	log.Debug("decryptSignParam")

	return query, nil
}

const (
	jsvarStr   = "[a-zA-Z_\\$][a-zA-Z_0-9]*"
	reverseStr = ":function\\(a\\)\\{" +
		"(?:return )?a\\.reverse\\(\\)" +
		"\\}"
	spliceStr = ":function\\(a,b\\)\\{" +
		"a\\.splice\\(0,b\\)" +
		"\\}"
	swapStr = ":function\\(a,b\\)\\{" +
		"var c=a\\[0\\];a\\[0\\]=a\\[b(?:%a\\.length)?\\];a\\[b(?:%a\\.length)?\\]=c(?:;return a)?" +
		"\\}"
)

var (
	nFunctionNameRegexp    = regexp.MustCompile("[a-zA-Z0-9]=\\[([a-zA-Z0-9]{3})\\];")
	signFunctionNameRegexp = regexp.MustCompile("function\\(([A-Za-z_0-9]+)\\)\\{([A-Za-z_0-9]+=[A-Za-z_0-9]+\\.split\\(\"\"\\)(.+?)\\.join\\(\"\"\\))\\}")
	actionsObjRegexp       = regexp.MustCompile(fmt.Sprintf(
		"var (%s)=\\{((?:(?:%s%s|%s%s|%s%s),?\\n?)+)\\};", jsvarStr, jsvarStr, swapStr, jsvarStr, spliceStr, jsvarStr, reverseStr))

	actionsFuncRegexp = regexp.MustCompile(fmt.Sprintf(
		"function(?: %s)?\\(a\\)\\{"+
			"a=a\\.split\\(\"\"\\);\\s*"+
			"((?:(?:a=)?%s\\.%s\\(a,\\d+\\);)+)"+
			"return a\\.join\\(\"\"\\)"+
			"\\}", jsvarStr, jsvarStr, jsvarStr))

	reverseRegexp = regexp.MustCompile(fmt.Sprintf("(?m)(?:^|,)(%s)%s", jsvarStr, reverseStr))
	spliceRegexp  = regexp.MustCompile(fmt.Sprintf("(?m)(?:^|,)(%s)%s", jsvarStr, spliceStr))
	swapRegexp    = regexp.MustCompile(fmt.Sprintf("(?m)(?:^|,)(%s)%s", jsvarStr, swapStr))
)

func (config playerConfig) decodeNsig(encoded string) (string, error) {
	fBody, err := config.getNFunction()
	if err != nil {
		return "", err
	}

	return evalJavascript(fBody, encoded)
}
func (config playerConfig) decodeSign(encoded string) (string, error) {
	fBody, err := config.getSignFunction()
	if err != nil {
		return "", err
	}

	return evalJavascript(fBody, encoded)
}

func evalJavascript(jsFunction, arg string) (string, error) {
	const myName = "myFunction"

	vm := goja.New()
	_, err := vm.RunString(myName + "=" + jsFunction)
	if err != nil {
		return "undefined", nil
	}

	var output func(string) string
	err = vm.ExportTo(vm.Get(myName), &output)
	if err != nil {
		return "undefined", nil
	}

	return output(arg), nil
}

func (config playerConfig) getNFunction() (string, error) {
	nameResult := nFunctionNameRegexp.FindSubmatch(config)
	if len(nameResult) == 0 {
		return "", errors.New("unable to extract n-function name")
	}

	var name = string(nameResult[1])

	return config.getStringBetweenStrings(fmt.Sprintf("%s=function\\(", name), "join\\(\"\"\\)\\};", 0), nil
}

func (config playerConfig) getSignFunction() (string, error) {
	match := signFunctionNameRegexp.FindSubmatch(config)
	if len(match) == 0 {
		return "", errors.New("unable to extract n-function name")
	}

	objReg, _ := regexp.Compile(`\.|\[`)
	varName := match[1]
	objNameArr := objReg.Split(string(match[3]), 2)
	objName := strings.TrimSpace(strings.Replace(string(objNameArr[0]), ";", "", -1))
	functions := config.getStringBetweenStrings(fmt.Sprintf(`var %s={`, objName), "};", 1)

	// function descramble_sig(k) { let Z1={r5:function(k,y){var q=k[0];k[0]=k[y%k.length];k[y%k.length]=q},\nw8:function(k,y){k.splice(0,y)},\nG9:function(k){k.reverse()}}; k=k.split("");Z1.w8(k,2);Z1.G9(k,62);Z1.r5(k,5);Z1.w8(k,1);Z1.G9(k,18);Z1.w8(k,2);Z1.G9(k,26);return k.join("") } descramble_sig(sig);
	return fmt.Sprintf(`function descramble_sig(%s) { let %s={%s}; %s };`, varName, objName, functions, match[2]), nil
}

func (config playerConfig) extraFunction(name string) (string, error) {
	// find the beginning of the function
	// def := []byte(name + "=function(")
	// start := bytes.Index(config, def)
	re, _ := regexp.Compile("[\n;]" + name + "=function\\(")
	start := re.FindIndex(config)[0]
	if start < 1 {
		return "", fmt.Errorf("unable to extract n-function body: looking for '%s'", re)
	}
	start++

	// start after the first curly bracket
	pos := start + bytes.IndexByte(config[start:], '{') + 1

	// find the bracket closing the function
	// for brackets := 1; brackets > 0; pos++ {
	// 	switch config[pos] {
	// 	case '{':
	// 		brackets++
	// 	case '}':
	// 		brackets--
	// 	}
	// }

	// match };
	for pos <= len(config)-1 {
		if string(config[pos-10:pos]) == "join(\"\")};" {
			break
		}
		pos++
	}

	return string(config[start:pos]), nil
}

func (config playerConfig) decrypt(cyphertext []byte) ([]byte, error) {
	operations, err := config.parseDecipherOps()
	if err != nil {
		return nil, err
	}

	// apply operations
	bs := []byte(cyphertext)
	for _, op := range operations {
		bs = op(bs)
	}

	return bs, nil
}

/*
parses decipher operations from https://youtube.com/s/player/4fbb4d5b/player_ias.vflset/en_US/base.js

var Mt={
splice:function(a,b){a.splice(0,b)},
reverse:function(a){a.reverse()},
EQ:function(a,b){var c=a[0];a[0]=a[b%a.length];a[b%a.length]=c}};

a=a.split("");
Mt.splice(a,3);
Mt.EQ(a,39);
Mt.splice(a,2);
Mt.EQ(a,1);
Mt.splice(a,1);
Mt.EQ(a,35);
Mt.EQ(a,51);
Mt.splice(a,2);
Mt.reverse(a,52);
return a.join("")
*/
func (config playerConfig) parseDecipherOps() (operations []DecipherOperation, err error) {
	objResult := actionsObjRegexp.FindSubmatch(config)
	funcResult := actionsFuncRegexp.FindSubmatch(config)
	if len(objResult) < 3 || len(funcResult) < 2 {
		return nil, fmt.Errorf("error parsing signature tokens (#obj=%d, #func=%d)", len(objResult), len(funcResult))
	}

	obj := objResult[1]
	objBody := objResult[2]
	funcBody := funcResult[1]

	var reverseKey, spliceKey, swapKey string

	if result := reverseRegexp.FindSubmatch(objBody); len(result) > 1 {
		reverseKey = string(result[1])
	}
	if result := spliceRegexp.FindSubmatch(objBody); len(result) > 1 {
		spliceKey = string(result[1])
	}
	if result := swapRegexp.FindSubmatch(objBody); len(result) > 1 {
		swapKey = string(result[1])
	}

	regex, err := regexp.Compile(fmt.Sprintf("(?:a=)?%s\\.(%s|%s|%s)\\(a,(\\d+)\\)", regexp.QuoteMeta(string(obj)), regexp.QuoteMeta(reverseKey), regexp.QuoteMeta(spliceKey), regexp.QuoteMeta(swapKey)))
	if err != nil {
		return nil, err
	}

	var ops []DecipherOperation
	for _, s := range regex.FindAllSubmatch(funcBody, -1) {
		switch string(s[1]) {
		case reverseKey:
			ops = append(ops, reverseFunc)
		case swapKey:
			arg, _ := strconv.Atoi(string(s[2]))
			ops = append(ops, newSwapFunc(arg))
		case spliceKey:
			arg, _ := strconv.Atoi(string(s[2]))
			ops = append(ops, newSpliceFunc(arg))
		}
	}
	return ops, nil
}

func (config playerConfig) getSignatureTimestamp() (string, error) {
	result := signatureRegexp.FindSubmatch(config)
	if result == nil {
		return "", ErrSignatureTimestampNotFound
	}

	return string(result[1]), nil
}

func (config playerConfig) getStringBetweenStrings(start string, end string, index int) string {
	var rege = regexp.MustCompile(`(?s)` + start + `(.*?)` + end)
	result := rege.FindSubmatch(config)
	if result == nil {
		return ""
	}

	return string(result[index])
}
