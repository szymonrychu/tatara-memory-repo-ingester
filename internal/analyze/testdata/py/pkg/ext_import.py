import requests
from flask import Flask

def handler():
    return requests.get("http://example.com")
