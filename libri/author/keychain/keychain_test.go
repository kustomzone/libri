package keychain

import (
	"io/ioutil"
	"os"
	"testing"

	"github.com/drausin/libri/libri/common/ecid"
	"github.com/stretchr/testify/assert"
)

func TestKeychain_Sample_ok(t *testing.T) {
	kc := New(3)
	k1, err := kc.Sample()
	assert.Nil(t, err)
	assert.NotNil(t, k1)
}

func TestKeychain_Sample_err(t *testing.T) {
	kc := New(0)
	k1, err := kc.Sample()
	assert.NotNil(t, err)
	assert.Nil(t, k1)
}

func TestKeychain_Get(t *testing.T) {
	kc := New(3)
	k1, _ := kc.Sample()
	assert.NotNil(t, k1)

	k1, in := kc.Get(ecid.ToPublicKeyBytes(k1))
	assert.True(t, in)
	assert.NotNil(t, k1)
}

func TestKeychain_Len(t *testing.T) {
	kc := New(3)
	assert.Equal(t, 3, kc.Len())
}

func TestSave_err(t *testing.T) {
	file, err := ioutil.TempFile("", "kechain-test")
	defer func() { assert.Nil(t, os.Remove(file.Name())) }()
	assert.Nil(t, err)
	assert.Nil(t, file.Close())

	// check error from bad scrypt params bubbles up
	err = Save(file.Name(), "test", New(3), -1, -1)
	assert.NotNil(t, err)
}

func TestLoad_err(t *testing.T) {
	file, err := ioutil.TempFile("", "kechain-test")
	defer func() { assert.Nil(t, os.Remove(file.Name())) }()
	assert.Nil(t, err)
	n, err := file.Write([]byte("not a keychain"))
	assert.Nil(t, err)
	assert.NotZero(t, n)
	assert.Nil(t, file.Close())

	// check that error from unmarshalling bad file bubbles up
	kc, err := Load(file.Name(), "test")
	assert.NotNil(t, err)
	assert.Nil(t, kc)
}

func TestSaveLoad(t *testing.T) {
	file, err := ioutil.TempFile("", "kechain-test")
	defer func() { assert.Nil(t, os.Remove(file.Name())) }()
	assert.Nil(t, err)
	assert.Nil(t, file.Close())

	kc1, auth := New(3), "test passphrase"
	err = Save(file.Name(), auth, kc1, veryLightScryptN, veryLightScryptP)
	assert.Nil(t, err)

	kc2, err := Load(file.Name(), auth)
	assert.Nil(t, err)
	assert.Equal(t, kc1, kc2)

	kc3, err := Load(file.Name(), "wrong passphrase")
	assert.NotNil(t, err)
	assert.Nil(t, kc3)

}
