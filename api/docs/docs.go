// Package docs Code generated by swaggo/swag. DO NOT EDIT
package docs

import "github.com/swaggo/swag"

const docTemplate = `{
    "schemes": {{ marshal .Schemes }},
    "swagger": "2.0",
    "info": {
        "description": "{{escape .Description}}",
        "title": "{{.Title}}",
        "contact": {},
        "version": "{{.Version}}"
    },
    "host": "{{.Host}}",
    "basePath": "{{.BasePath}}",
    "paths": {
        "/fullnodes": {
            "get": {
                "produces": [
                    "application/json"
                ],
                "tags": [
                    "fullnodes"
                ],
                "summary": "Get detected fullnodes peers",
                "parameters": [
                    {
                        "type": "string",
                        "description": "Last update",
                        "name": "lastUpdate",
                        "in": "query"
                    }
                ],
                "responses": {
                    "200": {
                        "description": "OK",
                        "schema": {
                            "type": "array",
                            "items": {
                                "$ref": "#/definitions/main.FullnodeApi"
                            }
                        }
                    }
                }
            }
        },
        "/syncstatus": {
            "get": {
                "produces": [
                    "application/json"
                ],
                "tags": [
                    "fullnodes"
                ],
                "summary": "Get number of fullnodes synced and not synced",
                "responses": {
                    "200": {
                        "description": "OK",
                        "schema": {
                            "type": "array",
                            "items": {
                                "$ref": "#/definitions/main.SyncCount"
                            }
                        }
                    }
                }
            }
        },
        "/versions": {
            "get": {
                "produces": [
                    "application/json"
                ],
                "tags": [
                    "fullnodes"
                ],
                "summary": "Get version run by fullnodes peers",
                "responses": {
                    "200": {
                        "description": "OK",
                        "schema": {
                            "type": "array",
                            "items": {
                                "$ref": "#/definitions/main.ClientVersionCount"
                            }
                        }
                    }
                }
            }
        }
    },
    "definitions": {
        "main.ClientVersionCount": {
            "type": "object",
            "properties": {
                "client_version": {
                    "type": "string"
                },
                "count": {
                    "type": "integer"
                }
            }
        },
        "main.FullnodeApi": {
            "type": "object",
            "properties": {
                "city": {
                    "type": "string"
                },
                "clientVersion": {
                    "type": "string"
                },
                "country": {
                    "type": "string"
                },
                "hostname": {
                    "type": "string"
                },
                "ip": {
                    "type": "string"
                },
                "isSynced": {
                    "type": "boolean"
                },
                "location": {
                    "type": "string"
                },
                "org": {
                    "type": "string"
                },
                "postal": {
                    "type": "string"
                },
                "region": {
                    "type": "string"
                },
                "timezone": {
                    "type": "string"
                },
                "updatedAt": {
                    "type": "string"
                }
            }
        },
        "main.SyncCount": {
            "type": "object",
            "properties": {
                "count": {
                    "type": "integer"
                },
                "isSynced": {
                    "type": "boolean"
                }
            }
        }
    }
}`

// SwaggerInfo holds exported Swagger Info so clients can modify it
var SwaggerInfo = &swag.Spec{
	Version:          "1.0",
	Host:             "map.alephium.notrustverify.ch",
	BasePath:         "/",
	Schemes:          []string{"https"},
	Title:            "Fullnodes Aggregator API",
	Description:      "Find connected fullnodes peers and get their approximate location",
	InfoInstanceName: "swagger",
	SwaggerTemplate:  docTemplate,
	LeftDelim:        "{{",
	RightDelim:       "}}",
}

func init() {
	swag.Register(SwaggerInfo.InstanceName(), SwaggerInfo)
}
