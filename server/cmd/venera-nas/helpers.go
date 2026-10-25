package main

import (
	"github.com/venera-app/venera-nas/internal/jshost"
	"github.com/venera-app/venera-nas/internal/model"
)

// fetchImage 下载单张图片。
func fetchImage(url string, headers map[string]string) ([]byte, error) {
	return jshost.FetchImage(nil, url, headers)
}

// transformOnResponse 执行源要求的字节级变换。
func transformOnResponse(initJS, funcSrc string, data []byte) ([]byte, error) {
	return jshost.TransformOnResponse(initJS, funcSrc, data)
}

// transformModifyImage 执行源要求的图片级变换。
func transformModifyImage(initJS, script string, data []byte) ([]byte, error) {
	return jshost.TransformModifyImage(initJS, script, data)
}

var _ = model.Comic{}
